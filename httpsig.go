// Copyright (c) 2021 James Bowes. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpsig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"errors"
	"io"
	"net/http"
	"time"
)

var defaultHeaders = []string{"content-type", "content-length"} // also method, path, query, and digest

func sliceHas(haystack []string, needle string) bool {
	for _, n := range haystack {
		if n == needle {
			return true
		}
	}

	return false
}

type Signer struct {
	signer
}

func NewSigner(opts ...signOption) *Signer {
	s := signer{
		keys:    make(map[string]sigHolder),
		nowFunc: time.Now,
	}

	for _, o := range opts {
		o.configureSign(&s)
	}

	if len(s.headers) == 0 {
		s.headers = defaultHeaders[:]
	}

	// TODO: normalize headers? lowercase & de-dupe

	// specialty components and digest first, for aesthetics
	for _, comp := range []string{"digest", "@query", "@path", "@method"} {
		if !sliceHas(s.headers, comp) {
			s.headers = append([]string{comp}, s.headers...)
		}
	}

	return &Signer{s}
}

func (s *Signer) Sign(r *http.Request) error {
	b := &bytes.Buffer{}
	if r.Body != nil {
		n, err := b.ReadFrom(r.Body)
		if err != nil {
			return err
		}
		r.Body.Close()

		if n != 0 {
			r.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
		}
	}

	// Always set a digest (for now)
	// TODO: we could skip setting digest on an empty body if content-length is included in the sig
	r.Header.Set("Digest", calcDigest(b.Bytes()))

	msg := messageFromRequest(r)
	hdr, err := s.signer.Sign(msg)
	if err != nil {
		return err
	}

	for k, v := range hdr {
		r.Header[k] = v
	}

	return nil
}

type VerifyingKey interface {
	Verify(data []byte, signature []byte) error
}

type VerifyingKeyResolver interface {
	Resolve(keyID string) VerifyingKey
}

type Verifier struct {
	*verifier
}

func NewVerifier(opts ...verifyOption) *Verifier {
	v := verifier{
		nowFunc: time.Now,
	}

	for _, o := range opts {
		o.configureVerify(&v)
	}

	return &Verifier{&v}
}

func (v *Verifier) Verify(r *http.Request) (keyID string, err error) {
	msg := messageFromRequest(r)
	keyID, err = v.verifier.Verify(msg)
	if err != nil {
		return keyID, err
	}

	b := &bytes.Buffer{}
	if r.Body != nil {
		n, err := b.ReadFrom(r.Body)
		if err != nil {
			return keyID, err
		}
		r.Body.Close()

		if n != 0 {
			r.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
		}
	}

	// Check the digest if set. We only support id-sha-256 for now.
	// TODO: option to require this?
	if dig := r.Header.Get("Digest"); dig != "" {
		if !verifyDigest(b.Bytes(), dig) {
			return keyID, errors.New("digest mismatch")
		}
	}
	return keyID, nil
}

// NewSignTransport returns a new client transport that wraps the provided transport with
// http message signing and body digest creation.
//
// Use the various `WithSign*` option funcs to configure signature algorithms with their provided
// key ids. You must provide at least one signing option. A signature for every provided key id is
// included on each request. Multiple included signatures allow you to gracefully introduce stronger
// algorithms, rotate keys, etc.
func NewSignTransport(transport http.RoundTripper, opts ...signOption) http.RoundTripper {
	s := NewSigner(opts...)

	return rt(func(r *http.Request) (*http.Response, error) {
		if err := s.Sign(r); err != nil {
			return nil, err
		}
		return transport.RoundTrip(r)
	})
}

type rt func(*http.Request) (*http.Response, error)

func (r rt) RoundTrip(req *http.Request) (*http.Response, error) { return r(req) }

// NewVerifyMiddleware returns a configured http server middleware that can be used to wrap
// multiple handlers for http message signature and digest verification.
//
// Use the `WithVerify*` option funcs to configure signature verification algorithms that map
// to their provided key ids.
//
// Requests with missing signatures, malformed signature headers, expired signatures, or
// invalid signatures are rejected with a `400` response. Only one valid signature is required
// from the known key ids. However, only the first known key id is checked.
func NewVerifyMiddleware(opts ...verifyOption) func(http.Handler) http.Handler {
	// TODO: form and multipart support
	v := NewVerifier(opts...)

	serveErr := func(rw http.ResponseWriter) {
		// TODO: better error and custom error handler
		rw.Header().Set("Content-Type", "text/plain")
		rw.WriteHeader(http.StatusBadRequest)

		_, _ = rw.Write([]byte("invalid required signature"))
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if _, err := v.Verify(r); err != nil {
				serveErr(rw)
				return
			}
			h.ServeHTTP(rw, r)
		})
	}
}

type signOption interface {
	configureSign(s *signer)
}

type verifyOption interface {
	configureVerify(v *verifier)
}

type signOrVerifyOption interface {
	signOption
	verifyOption
}

type optImpl struct {
	s func(s *signer)
	v func(v *verifier)
}

func (o *optImpl) configureSign(s *signer)     { o.s(s) }
func (o *optImpl) configureVerify(v *verifier) { o.v(v) }

// WithHeaders sets the list of headers that will be included in the signature.
// The Digest header is always included (and the digest calculated).
//
// If not provided, the default headers `content-type, content-length, host` are used.
func WithHeaders(hdr ...string) signOption {
	// TODO: use this to implement required headers in verify?
	return &optImpl{
		s: func(s *signer) { s.headers = hdr },
	}
}

func WithVerifyingKeyResolver(resolver VerifyingKeyResolver) verifyOption {
	return &optImpl{
		v: func(v *verifier) { v.resolver = resolver },
	}
}

// WithSignRsaPssSha512 adds signing using `rsa-pss-sha512` with the given private key
// using the given key id.
func WithSignRsaPssSha512(keyID string, pk *rsa.PrivateKey) signOption {
	return &optImpl{
		s: func(s *signer) { s.keys[keyID] = signRsaPssSha512(pk) },
	}
}

// WithVerifyRsaPssSha512 adds signature verification using `rsa-pss-sha512` with the
// given public key using the given key id.
func WithVerifyRsaPssSha512(keyID string, pk *rsa.PublicKey) verifyOption {
	return &optImpl{
		v: func(v *verifier) { v.keys.Store(keyID, verifyRsaPssSha512(pk)) },
	}
}

// WithSignEcdsaP256Sha256 adds signing using `ecdsa-p256-sha256` with the given private key
// using the given key id.
func WithSignEcdsaP256Sha256(keyID string, pk *ecdsa.PrivateKey) signOption {
	return &optImpl{
		s: func(s *signer) { s.keys[keyID] = signEccP256(pk) },
	}
}

// WithVerifyEcdsaP256Sha256 adds signature verification using `ecdsa-p256-sha256` with the
// given public key using the given key id.
func WithVerifyEcdsaP256Sha256(keyID string, pk *ecdsa.PublicKey) verifyOption {
	return &optImpl{
		v: func(v *verifier) { v.keys.Store(keyID, verifyEccP256(pk)) },
	}
}

// WithHmacSha256 adds signing or signature verification using `hmac-sha256` with the
// given shared secret using the given key id.
func WithHmacSha256(keyID string, secret []byte) signOrVerifyOption {
	return &optImpl{
		s: func(s *signer) { s.keys[keyID] = signHmacSha256(secret) },
		v: func(v *verifier) { v.keys.Store(keyID, verifyHmacSha256(secret)) },
	}
}
