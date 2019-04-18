package provisioner

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/cli/jose"
)

const (
	defaultCacheAge    = 12 * time.Hour
	defaultCacheJitter = 1 * time.Hour
)

var maxAgeRegex = regexp.MustCompile("max-age=([0-9]*)")

type oauth2Certificate struct {
	ID          string
	Certificate *x509.Certificate
}

type oauth2CertificateSet struct {
	Certificates []oauth2Certificate
}

func (s oauth2CertificateSet) Get(id string) *x509.Certificate {
	for _, c := range s.Certificates {
		if c.ID == id {
			return c.Certificate
		}
	}
	return nil
}

type keyStore struct {
	sync.RWMutex
	uri     string
	keySet  jose.JSONWebKeySet
	certSet oauth2CertificateSet
	timer   *time.Timer
	expiry  time.Time
	jitter  time.Duration
}

func newKeyStore(uri string) (*keyStore, error) {
	keys, age, err := getKeysFromJWKsURI(uri)
	if err != nil {
		return nil, err
	}
	ks := &keyStore{
		uri:    uri,
		keySet: keys,
		expiry: getExpirationTime(age),
		jitter: getCacheJitter(age),
	}
	next := ks.nextReloadDuration(age)
	ks.timer = time.AfterFunc(next, ks.reload)
	return ks, nil
}

func newCertificateStore(uri string) (*keyStore, error) {
	certs, age, err := getOauth2Certificates(uri)
	if err != nil {
		return nil, err
	}
	ks := &keyStore{
		uri:     uri,
		certSet: certs,
		expiry:  getExpirationTime(age),
		jitter:  getCacheJitter(age),
	}
	next := ks.nextReloadDuration(age)
	ks.timer = time.AfterFunc(next, ks.reloadCertificates)
	return ks, nil
}

func (ks *keyStore) Close() {
	ks.timer.Stop()
}

func (ks *keyStore) Get(kid string) (keys []jose.JSONWebKey) {
	ks.RLock()
	// Force reload if expiration has passed
	if time.Now().After(ks.expiry) {
		ks.RUnlock()
		ks.reload()
		ks.RLock()
	}
	keys = ks.keySet.Key(kid)
	ks.RUnlock()
	return
}

func (ks *keyStore) GetCertificate(kid string) (cert *x509.Certificate) {
	ks.RLock()
	// Force reload if expiration has passed
	if time.Now().After(ks.expiry) {
		ks.RUnlock()
		ks.reloadCertificates()
		ks.RLock()
	}
	cert = ks.certSet.Get(kid)
	ks.RUnlock()
	return
}

func (ks *keyStore) reload() {
	var next time.Duration
	keys, age, err := getKeysFromJWKsURI(ks.uri)
	if err != nil {
		next = ks.nextReloadDuration(ks.jitter / 2)
	} else {
		ks.Lock()
		ks.keySet = keys
		ks.expiry = getExpirationTime(age)
		ks.jitter = getCacheJitter(age)
		next = ks.nextReloadDuration(age)
		ks.Unlock()
	}

	ks.Lock()
	ks.timer.Reset(next)
	ks.Unlock()
}

func (ks *keyStore) reloadCertificates() {
	var next time.Duration
	certs, age, err := getOauth2Certificates(ks.uri)
	if err != nil {
		next = ks.nextReloadDuration(ks.jitter / 2)
	} else {
		ks.Lock()
		ks.certSet = certs
		ks.expiry = getExpirationTime(age)
		ks.jitter = getCacheJitter(age)
		next = ks.nextReloadDuration(age)
		ks.Unlock()
	}

	ks.Lock()
	ks.timer.Reset(next)
	ks.Unlock()
}

func (ks *keyStore) nextReloadDuration(age time.Duration) time.Duration {
	n := rand.Int63n(int64(ks.jitter))
	age -= time.Duration(n)
	if age < 0 {
		age = 0
	}
	return age
}

func getKeysFromJWKsURI(uri string) (jose.JSONWebKeySet, time.Duration, error) {
	var keys jose.JSONWebKeySet
	resp, err := http.Get(uri)
	if err != nil {
		return keys, 0, errors.Wrapf(err, "failed to connect to %s", uri)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return keys, 0, errors.Wrapf(err, "error reading %s", uri)
	}
	return keys, getCacheAge(resp.Header.Get("cache-control")), nil
}

func getOauth2Certificates(uri string) (oauth2CertificateSet, time.Duration, error) {
	var certs oauth2CertificateSet
	resp, err := http.Get(uri)
	if err != nil {
		return certs, 0, errors.Wrapf(err, "failed to connect to %s", uri)
	}
	defer resp.Body.Close()
	m := make(map[string]string)
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return certs, 0, errors.Wrapf(err, "error reading %s", uri)
	}
	for k, v := range m {
		block, _ := pem.Decode([]byte(v))
		if block == nil || block.Type != "CERTIFICATE" {
			return certs, 0, errors.Wrapf(err, "error parsing certificate %s from %s", k, uri)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return certs, 0, errors.Wrapf(err, "error parsing certificate %s from %s", k, uri)
		}
		certs.Certificates = append(certs.Certificates, oauth2Certificate{
			ID:          k,
			Certificate: cert,
		})
	}
	return certs, getCacheAge(resp.Header.Get("cache-control")), nil
}

func getCacheAge(cacheControl string) time.Duration {
	age := defaultCacheAge
	if len(cacheControl) > 0 {
		match := maxAgeRegex.FindAllStringSubmatch(cacheControl, -1)
		if len(match) > 0 {
			if len(match[0]) == 2 {
				maxAge := match[0][1]
				maxAgeInt, err := strconv.ParseInt(maxAge, 10, 64)
				if err != nil {
					return defaultCacheAge
				}
				age = time.Duration(maxAgeInt) * time.Second
			}
		}
	}
	return age
}

func getCacheJitter(age time.Duration) time.Duration {
	switch {
	case age > time.Hour:
		return defaultCacheJitter
	default:
		return age / 3
	}
}

func getExpirationTime(age time.Duration) time.Time {
	return time.Now().Truncate(time.Second).Add(age)
}
