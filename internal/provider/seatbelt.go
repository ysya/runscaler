package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

// SeatbeltProvider runs trusted jobs directly on macOS. It isolates files, not
// the kernel, ports, resource quotas or deliberately hostile same-user code.
type SeatbeltProvider struct {
	cfg       config.SeatbeltConfig
	mu        sync.Mutex
	instances map[string]*seatbeltInstance
}
type seatbeltInstance struct {
	cmd         *exec.Cmd
	done        chan struct{}
	err         error
	dir, marker string
	log         *os.File
}

func NewSeatbeltProvider(ss config.ScaleSetConfig) *SeatbeltProvider {
	return &SeatbeltProvider{cfg: ss.Seatbelt, instances: make(map[string]*seatbeltInstance)}
}

func (p *SeatbeltProvider) Validate() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("seatbelt requires macOS")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return err
	}
	for _, path := range append([]string{p.cfg.RunnerDir, p.cfg.StateDir}, p.cfg.ReadOnlyPaths...) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return fmt.Errorf("seatbelt path must be a clean absolute directory: %q", path)
		}
	}
	if p.cfg.StateDir == p.cfg.RunnerDir || strings.HasPrefix(p.cfg.StateDir, p.cfg.RunnerDir+"/") || strings.HasPrefix(p.cfg.RunnerDir, p.cfg.StateDir+"/") {
		return fmt.Errorf("seatbelt template and state roots must be disjoint")
	}
	for _, path := range p.cfg.ReadOnlyPaths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if resolved != path {
			return fmt.Errorf("seatbelt read-only paths must be canonical")
		}
		if path == "/Users" || path == "/private" || path == "/tmp" || path == "/private/tmp" || path == p.cfg.StateDir || strings.HasPrefix(p.cfg.StateDir, path+"/") {
			return fmt.Errorf("seatbelt read-only path exposes private job state")
		}
	}
	for _, path := range []string{p.cfg.SDKRoot, p.cfg.DeveloperDir} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/") {
			return fmt.Errorf("seatbelt SDK and developer paths must be absolute")
		}
	}
	for key, value := range p.cfg.Environment {
		key = strings.ToUpper(key)
		if key != "ANDROID_HOME" && key != "ANDROID_SDK_ROOT" && key != "JAVA_HOME" {
			return fmt.Errorf("unsupported seatbelt environment key %q", key)
		}
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("seatbelt tool environment requires absolute paths")
		}
	}
	template, err := filepath.EvalSymlinks(p.cfg.RunnerDir)
	if err != nil {
		return err
	}
	if template != p.cfg.RunnerDir {
		return fmt.Errorf("seatbelt runner-dir must not contain symlinks")
	}
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if _, err := os.Lstat(filepath.Join(template, name)); err == nil {
			return fmt.Errorf("seatbelt runner template must be unconfigured")
		}
	}
	for _, name := range []string{"run.sh", "bin/Runner.Listener"} {
		if _, err := os.Stat(filepath.Join(template, name)); err != nil {
			return err
		}
	}
	return nil
}

// SeatbeltProfile keeps the job's entire writable tree private. Tools outside macOS's
// system directories need explicit read-only allowlisting. No ambient HOME or
// controller credentials are exposed to the job. userDirs are the job's own
// per-user Darwin cache/temp directories (see seatbeltUserDirs); signals are
// limited to processes inside the same sandbox, so a job can stop its own
// children but nothing else.
func SeatbeltProfile(dir string, tools []string, userDirs ...string) string {
	q := strconv.Quote
	s := "(version 1)\n(allow default)\n(allow process*)\n(allow sysctl-read)\n(allow mach-lookup)\n(allow network*)\n(allow system-info)\n(allow user-preference-read (preference-domain \"com.apple.dt.Xcode\") (preference-domain \"com.apple.dt.xcode\") (preference-domain \"com.apple.dt.xcodebuild\"))\n(allow ipc-posix-shm-read*)\n"
	for _, path := range append([]string{"/System", "/System/Volumes/Preboot", "/private/preboot", "/usr", "/bin", "/sbin", "/Library/Developer", "/Library/Apple", "/Library/Preferences/com.apple.dt.Xcode.plist", "/private/var/db", "/private/etc", "/dev"}, tools...) {
		s += "(allow file-read* (subpath " + q(path) + "))\n"
	}
	for parent := filepath.Dir(dir); parent != "/"; parent = filepath.Dir(parent) {
		s += "(allow file-read* (literal " + q(parent) + "))\n"
	}
	s += "(allow file* (subpath " + q(dir) + "))\n"
	s += "(allow file-read* file-write* (subpath " + q(dir) + "))\n(allow file-write-create file-write-data file-write-unlink (subpath " + q(dir) + "))\n(allow file-map-executable (subpath " + q(dir) + "))\n(allow file-ioctl (subpath " + q(dir) + "))\n(allow file-read-metadata)\n(allow file-read-data (literal \"/\"))\n(allow file-write* (literal \"/dev/null\"))\n"
	// SwiftPM, clang and the linker may use the host temp roots even when
	// TMPDIR and Darwin user dirs are redirected into the job tree.
	for _, tempDir := range []string{"/private/tmp", "/private/var/tmp"} {
		s += "(allow file-read* file-write* (subpath " + q(tempDir) + "))\n"
	}
	for _, userDir := range userDirs {
		s += "(allow file-read* file-write* (subpath " + q(userDir) + "))\n"
		s += "(allow file-read* (subpath " + q(filepath.Dir(userDir)) + "))\n"
		tempItems := filepath.Join(filepath.Dir(userDir), "TemporaryItems")
		s += "(allow file-write* (subpath " + q(tempItems) + "))\n(allow file-write-create file-write-data file-write-unlink (subpath " + q(tempItems) + "))\n"
	}
	s += "(allow signal (target same-sandbox))\n"
	return s
}

// seatbeltUserDirSuffix names the job's private subdirectory of the per-user
// Darwin cache and temp directories. confstr(3) appends
// DIRHELPER_USER_DIR_SUFFIX to _CS_DARWIN_USER_{CACHE,TEMP}_DIR, which is where
// clang and swiftc place implicit module caches regardless of TMPDIR.
func seatbeltUserDirSuffix(dir string) string {
	return "runscaler-" + filepath.Base(dir)
}

// seatbeltUserDirs resolves the job's private Darwin user directories. The
// sandbox matches resolved paths, so /var/folders is reported as
// /private/var/folders.
func seatbeltUserDirs(dir string) ([]string, error) {
	var dirs []string
	for _, name := range []string{"DARWIN_USER_CACHE_DIR", "DARWIN_USER_TEMP_DIR"} {
		out, err := exec.Command("/usr/bin/getconf", name).Output()
		if err != nil {
			return nil, fmt.Errorf("getconf %s: %w", name, err)
		}
		base, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		if err != nil {
			return nil, err
		}
		if base == "/" || !strings.HasPrefix(base, "/private/var/folders/") {
			return nil, fmt.Errorf("unexpected %s %q", name, base)
		}
		dirs = append(dirs, filepath.Join(base, seatbeltUserDirSuffix(dir)))
	}
	return dirs, nil
}

func (p *SeatbeltProvider) StartInstance(ctx context.Context, name, jit string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	if jit == "" || strings.ContainsAny(jit, "\r\n") {
		return "", fmt.Errorf("invalid JIT configuration")
	}
	if err := os.MkdirAll(p.cfg.StateDir, 0700); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(p.cfg.StateDir)
	if err != nil {
		return "", err
	}
	if root != p.cfg.StateDir {
		return "", fmt.Errorf("seatbelt state-dir must not contain symlinks")
	}
	// Random, controller-owned paths never incorporate GitHub input.
	dir, err := os.MkdirTemp(root, "job-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	copyCmd := exec.CommandContext(ctx, "/bin/cp", "-cR", p.cfg.RunnerDir+"/.", dir)
	if err := copyCmd.Run(); err != nil {
		return "", fmt.Errorf("copy runner template: %w", err)
	}
	for _, f := range []string{".runner", ".credentials", ".credentials_rsaparams", ".env", ".path"} {
		_ = os.Remove(filepath.Join(dir, f))
	}
	for _, d := range []string{"_work", "tmp", "home"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0700); err != nil {
			return "", err
		}
	}
	// GitHub-hosted macOS runners expose rustup and its cargo bin directory to
	// every step. Keep that contract when the job gets its own HOME/CARGO_HOME:
	// reuse the host rustup executable, but place its proxy and installed
	// toolchains under the job's private cargo directory.
	cargoBin := filepath.Join(dir, "home", ".cargo", "bin")
	if err := os.MkdirAll(cargoBin, 0700); err != nil {
		return "", err
	}
	if rustup, err := executableInPath(p.cfg.Path, "rustup"); err == nil {
		if err := os.Symlink(rustup, filepath.Join(cargoBin, "rustup")); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	userDirs, err := seatbeltUserDirs(dir)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.sb"), []byte(SeatbeltProfile(dir, p.cfg.ReadOnlyPaths, userDirs...)), 0600); err != nil {
		return "", err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	marker := "RUNSCALER_SEATBELT_JOB=" + hex.EncodeToString(token)
	log, err := os.OpenFile(filepath.Join(dir, "runner.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", filepath.Join(dir, "profile.sb"), filepath.Join(dir, "run.sh"))
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + cargoBin + ":" + p.cfg.Path, "HOME=" + filepath.Join(dir, "home"), "CFFIXED_USER_HOME=" + filepath.Join(dir, "home"), "TMPDIR=" + filepath.Join(dir, "tmp"), "DIRHELPER_USER_DIR_SUFFIX=" + seatbeltUserDirSuffix(dir), "LANG=en_US.UTF-8", "xcrun_nocache=1", "RUNSCALER_PROVIDER=seatbelt", "ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit, marker}
	if p.cfg.SDKRoot != "" {
		cmd.Env = append(cmd.Env, "SDKROOT="+p.cfg.SDKRoot)
	}
	if p.cfg.DeveloperDir != "" {
		cmd.Env = append(cmd.Env, "DEVELOPER_DIR="+p.cfg.DeveloperDir)
	}
	// Android tools and workflows expect a writable SDK under the private HOME.
	// APFS clones retain shared blocks while isolating SDK updates per job.
	androidSource := ""
	for key, value := range p.cfg.Environment {
		if strings.ToUpper(key) == "ANDROID_HOME" || strings.ToUpper(key) == "ANDROID_SDK_ROOT" {
			if androidSource != "" && androidSource != value {
				log.Close()
				return "", fmt.Errorf("android SDK roots must agree")
			}
			androidSource = value
		}
	}
	if androidSource != "" {
		androidDir := filepath.Join(dir, "home", "android-sdk")
		if err := exec.CommandContext(ctx, "/bin/cp", "-cR", androidSource, androidDir).Run(); err != nil {
			log.Close()
			return "", fmt.Errorf("clone Android SDK: %w", err)
		}
		cmd.Env = append(cmd.Env, "ANDROID_HOME="+androidDir, "ANDROID_SDK_ROOT="+androidDir)
	}
	for key, value := range p.cfg.Environment {
		if strings.ToUpper(key) == "JAVA_HOME" {
			cmd.Env = append(cmd.Env, "JAVA_HOME="+value)
		}
	}
	cmd.Stdout = log
	cmd.Stderr = log
	seatbeltProcessGroup(cmd)
	if err := ctx.Err(); err != nil {
		log.Close()
		return "", err
	}
	if err := cmd.Start(); err != nil {
		log.Close()
		return "", err
	}
	inst := &seatbeltInstance{cmd: cmd, done: make(chan struct{}), dir: dir, marker: marker, log: log}
	record, _ := json.Marshal(struct {
		PID    int
		Marker string
	}{cmd.Process.Pid, marker})
	if err := os.WriteFile(dir+".json", record, 0600); err != nil {
		_ = seatbeltKill(context.WithoutCancel(ctx), inst)
		_ = cmd.Wait()
		log.Close()
		return "", err
	}
	p.mu.Lock()
	p.instances[dir] = inst
	p.mu.Unlock()
	go func() { inst.err = cmd.Wait(); _ = log.Close(); close(inst.done) }()
	ok = true
	return dir, nil
}

func executableInPath(pathValue, name string) (string, error) {
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}
func (p *SeatbeltProvider) instance(id string) *seatbeltInstance {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.instances[id]
}
func (p *SeatbeltProvider) WaitInstance(ctx context.Context, id string) error {
	i := p.instance(id)
	if i == nil {
		return nil
	}
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *SeatbeltProvider) WaitReady(ctx context.Context, id string) error {
	i := p.instance(id)
	if i == nil {
		return ErrExitedBeforeReady
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		data, _ := os.ReadFile(filepath.Join(i.dir, "runner.log"))
		if strings.Contains(string(data), "Listening for Jobs") || strings.Contains(string(data), "Running job:") {
			return nil
		}
		select {
		case <-i.done:
			return ErrExitedBeforeReady
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
func (p *SeatbeltProvider) RemoveInstance(ctx context.Context, id string) error {
	i := p.instance(id)
	if i == nil {
		return nil
	}
	if err := seatbeltKill(ctx, i); err != nil {
		return err
	}
	select {
	case <-i.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	userDirs, err := seatbeltUserDirs(i.dir)
	if err != nil {
		return err
	}
	profile := filepath.Join(i.dir, "profile.sb")
	for _, userDir := range userDirs {
		// Darwin owns the per-user root and marks it sunlnk. Clean only the
		// job's children from inside the same sandbox; removing the root itself
		// is rejected by macOS and would strand the runner allocation.
		cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-f", profile, "/bin/sh", "-c", "find \"$1\" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +", "sh", userDir)
		_ = cmd.Run()
		if err := os.RemoveAll(userDir); err != nil && !errors.Is(err, os.ErrNotExist) && !os.IsPermission(err) {
			return err
		}
	}
	// Go module caches contain read-only directories; restore owner write bits
	// only beneath our randomly allocated job root before removing it.
	if err := filepath.WalkDir(i.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0700)
		}
		return nil
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(i.dir); err != nil {
		return err
	}
	if err := os.Remove(i.dir + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p.mu.Lock()
	delete(p.instances, id)
	p.mu.Unlock()
	return nil
}
func (p *SeatbeltProvider) Shutdown(ctx context.Context) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.instances))
	for id := range p.instances {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		_ = p.RemoveInstance(ctx, id)
	}
}

// Reconcile removes only receipt-bound instances from this configured private
// root. Receipts live outside the sandbox-writable job tree.
func (p *SeatbeltProvider) Reconcile(ctx context.Context) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(p.cfg.StateDir, 0700); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(p.cfg.StateDir)
	if err != nil {
		return err
	}
	records, err := filepath.Glob(filepath.Join(root, "job-*.json"))
	if err != nil {
		return err
	}
	for _, path := range records {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record struct {
			PID    int
			Marker string
		}
		if err = json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.PID < 1 || !strings.HasPrefix(record.Marker, "RUNSCALER_SEATBELT_JOB=") {
			return fmt.Errorf("invalid Seatbelt recovery receipt")
		}
		dir := strings.TrimSuffix(path, ".json")
		// Recovery validates the live command before using a saved group ID.
		inst := &seatbeltInstance{dir: dir, marker: record.Marker, cmd: &exec.Cmd{Process: &os.Process{Pid: record.PID}}, done: make(chan struct{})}
		close(inst.done)
		p.mu.Lock()
		p.instances[dir] = inst
		p.mu.Unlock()
		if err := p.RemoveInstance(ctx, dir); err != nil {
			return err
		}
	}
	return nil
}
