package actionscache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

type ServiceStatus struct {
	Ready    bool
	Degraded bool
	Entries  int64
	Bytes    int64
}

type Service struct {
	cfg       config.ActionsCacheConfig
	logger    *slog.Logger
	store     *Store
	verifier  *Verifier
	server    *http.Server
	listen    func(string, string) (net.Listener, error)
	ready     atomic.Bool
	degraded  atomic.Bool
	stopSweep context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex
	closed    bool
	failures  chan error
}

type reportingListener struct {
	net.Listener
	onFailure func(error)
}

func (l reportingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		l.onFailure(err)
	}
	return conn, err
}

func NewService(cfg config.ActionsCacheConfig, logger *slog.Logger) (*Service, error) {
	if err := cfg.Validate(0); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("native cache is disabled")
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open native cache store: %w", err)
	}
	store.SetStagingLimit(cfg.MaxSizeBytes)
	verifier, err := NewVerifier(VerifierConfig{Issuer: cfg.EffectiveTokenIssuer(), JWKSURL: cfg.EffectiveJWKSURL(), Audiences: cfg.Audiences})
	if err != nil {
		store.Close()
		return nil, err
	}
	service := &Service{cfg: cfg, logger: logger, store: store, verifier: verifier, listen: net.Listen, failures: make(chan error, 1)}
	handler, err := NewHandler(HandlerConfig{
		Store: store, Verifier: verifier, BaseURL: cfg.AdvertiseURL, UpstreamURL: cfg.EffectiveUpstreamResultsURL(),
		MaxSizeBytes: cfg.MaxSizeBytes, MaxIdle: time.Duration(cfg.EffectiveMaxIdleDays()) * 24 * time.Hour,
		OnCleanupError: func(err error) {
			service.degraded.Store(true)
			logger.Error("Native cache cleanup failed", slog.Any("error", err))
		},
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	service.server = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	return service, nil
}

func (s *Service) Start(ctx context.Context) error {
	if err := s.verifier.Bootstrap(ctx); err != nil {
		return fmt.Errorf("cache token keys unavailable: %w", err)
	}
	if err := s.store.Sweep(ctx, s.cfg.MaxSizeBytes, time.Duration(s.cfg.EffectiveMaxIdleDays())*24*time.Hour); err != nil {
		return fmt.Errorf("cache cleanup failed: %w", err)
	}
	listener, err := s.listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("cache listener unavailable: %w", err)
	}
	s.ready.Store(true)
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	s.stopSweep = stopSweep
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		wrapped := reportingListener{Listener: listener, onFailure: func(err error) {
			if s.ready.Load() {
				s.reportFailure(err)
				go func() { _ = s.server.Close() }()
			}
		}}
		if err := s.server.Serve(wrapped); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.reportFailure(err)
		}
	}()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(sweepCtx, 2*time.Minute)
				err := s.store.Sweep(ctx, s.cfg.MaxSizeBytes, time.Duration(s.cfg.EffectiveMaxIdleDays())*24*time.Hour)
				cancel()
				if err != nil {
					s.degraded.Store(true)
					s.logger.Error("Native cache cleanup failed", slog.Any("error", err))
				} else {
					s.degraded.Store(false)
				}
			}
		}
	}()
	return nil
}

func (s *Service) Failures() <-chan error { return s.failures }

func (s *Service) reportFailure(err error) {
	if !s.ready.Swap(false) {
		return
	}
	s.degraded.Store(true)
	s.logger.Error("Native cache listener failed", slog.Any("error", err))
	select {
	case s.failures <- err:
	default:
	}
}

func (s *Service) Status() ServiceStatus {
	status := ServiceStatus{Ready: s.ready.Load(), Degraded: s.degraded.Load()}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return status
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stats, err := s.store.Stats(ctx)
	if err != nil {
		status.Degraded = true
		return status
	}
	status.Entries, status.Bytes = stats.Entries, stats.Bytes
	return status
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.ready.Store(false)
	if s.stopSweep != nil {
		s.stopSweep()
	}
	s.mu.Unlock()
	err := s.server.Shutdown(ctx)
	if err != nil {
		_ = s.server.Close()
	}
	s.wg.Wait()
	if closeErr := s.store.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (s *Service) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}
