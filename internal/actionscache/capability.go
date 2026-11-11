package actionscache

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
)

type capabilitySigner struct {
	key  []byte
	base *url.URL
}

func newCapabilitySigner(root, baseURL string) (*capabilitySigner, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "/" {
		return nil, errors.New("invalid cache advertise URL")
	}
	path := filepath.Join(root, "capability.key")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			_, err = file.Write(key)
		}
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
	} else if !os.IsExist(err) {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("invalid cache capability key")
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid cache capability key")
	}
	return &capabilitySigner{key: key, base: base}, nil
}

func (s *capabilitySigner) url(operation, id string, lifetime time.Duration) string {
	expiry := time.Now().Add(lifetime).Unix()
	path := "/download/" + id
	if operation == "upload" {
		path = "/devstoreaccount1/upload/" + id
	}
	mac := s.mac(operation, id, expiry)
	u := *s.base
	u.Path = path
	u.RawQuery = "exp=" + strconv.FormatInt(expiry, 10) + "&sig=" + base64.RawURLEncoding.EncodeToString(mac)
	return u.String()
}

func (s *capabilitySigner) verify(operation, id string, query url.Values) error {
	if _, err := uuid.Parse(id); err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
		return errors.New("invalid capability")
	}
	expiry, err := strconv.ParseInt(query.Get("exp"), 10, 64)
	if err != nil || expiry < time.Now().Unix() || expiry > time.Now().Add(2*time.Hour).Unix()+60 {
		return errors.New("expired capability")
	}
	provided, err := base64.RawURLEncoding.DecodeString(query.Get("sig"))
	if err != nil || !hmac.Equal(provided, s.mac(operation, id, expiry)) {
		return errors.New("invalid capability")
	}
	return nil
}

func (s *capabilitySigner) mac(operation, id string, expiry int64) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%d", operation, id, expiry)
	return mac.Sum(nil)
}
