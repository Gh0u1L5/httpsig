// Copyright (c) 2021 James Bowes. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpsig

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type verImpl struct {
	w      io.Writer
	verify func([]byte) error
}

type verHolder struct {
	alg      string
	verifier func() verImpl
}

type verifier struct {
	keys     sync.Map // map[string]verHolder
	resolver VerifyingKeyResolver

	// For testing
	nowFunc func() time.Time
}

// XXX: note about fail fast.
func (v *verifier) Verify(msg *message) (keyID string, err error) {
	sigHdr := msg.Header.Get("Signature")
	if sigHdr == "" {
		return "", errNotSigned
	}

	paramHdr := msg.Header.Get("Signature-Input")
	if paramHdr == "" {
		return "", errNotSigned
	}

	sigParts := strings.Split(sigHdr, ", ")
	paramParts := strings.Split(paramHdr, ", ")

	if len(sigParts) != len(paramParts) {
		return "", errMalformedSignature
	}

	// TODO: could be smarter about selecting the sig to verify, eg based
	// on algorithm
	var sigID string
	var params *signatureParams
	var paramsRaw string
	for _, p := range paramParts {
		pParts := strings.SplitN(p, "=", 2)
		if len(pParts) != 2 {
			return "", errMalformedSignature
		}

		candidate, err := parseSignatureInput(pParts[1])
		if err != nil {
			return "", errMalformedSignature
		}

		if _, ok := v.ResolveKey(candidate.keyID); ok {
			sigID = pParts[0]
			params = candidate
			paramsRaw = pParts[1]
			break
		}
	}

	if params == nil {
		return "", errUnknownKey
	}

	var signature string
	for _, s := range sigParts {
		sParts := strings.SplitN(s, "=", 2)
		if len(sParts) != 2 {
			return params.keyID, errMalformedSignature
		}

		if sParts[0] == sigID {
			// TODO: error if not surrounded by colons
			signature = strings.Trim(sParts[1], ":")
			break
		}
	}

	if signature == "" {
		return params.keyID, errMalformedSignature
	}

	ver, _ := v.ResolveKey(params.keyID)
	if ver.alg != "" && params.alg != "" && ver.alg != params.alg {
		return params.keyID, errAlgMismatch
	}

	// verify signature. if invalid, error
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return params.keyID, errMalformedSignature
	}

	verifier := ver.verifier()

	//TODO: skip the buffer.

	var b bytes.Buffer

	// canonicalize headers
	// TODO: wrap the errors within
	for _, h := range params.items {

		// handle specialty components, section 2.3
		var err error
		switch h {
		case "@method":
			err = canonicalizeMethod(&b, msg.Method)
		case "@path":
			err = canonicalizePath(&b, msg.URL.Path)
		case "@query":
			err = canonicalizeQuery(&b, msg.URL.RawQuery)
		case "@authority":
			err = canonicalizeAuthority(&b, msg.Authority)
		default:
			// handle default (header) components
			err = canonicalizeHeader(&b, h, msg.Header)
		}

		if err != nil {
			return params.keyID, err
		}
	}
	fmt.Fprintf(&b, "\"@signature-params\": %s", paramsRaw)

	if _, err := verifier.w.Write(b.Bytes()); err != nil {
		return params.keyID, err
	}

	err = verifier.verify(sig)
	if err != nil {
		return params.keyID, errInvalidSignature
	}

	// TODO: could put in some wiggle room
	if params.expires != nil && params.expires.After(time.Now()) {
		return params.keyID, errSignatureExpired
	}

	return params.keyID, nil
}

func (v *verifier) ResolveKey(keyID string) (verHolder, bool) {
	if holder, ok := v.keys.Load(keyID); ok {
		return holder.(verHolder), true
	}

	if v.resolver != nil {
		key := v.resolver.Resolve(keyID)
		if key != nil {
			holder := verHolder{
				verifier: func() verImpl {
					in := bytes.NewBuffer(make([]byte, 0, 1024))
					return verImpl{
						w: in,
						verify: func(sig []byte) error {
							return key.Verify(in.Bytes(), sig)
						},
					}
				},
			}
			v.keys.Store(keyID, holder)
			return holder, true
		}
	}

	return verHolder{}, false
}

// XXX use vice here too.

var (
	errNotSigned          = errors.New("signature headers not found")
	errMalformedSignature = errors.New("unable to parse signature headers")
	errUnknownKey         = errors.New("unknown key id")
	errAlgMismatch        = errors.New("algorithm mismatch for key id")
	errSignatureExpired   = errors.New("signature expired")
	errInvalidSignature   = errors.New("invalid signature")
)

// These error checking funcs aren't needed yet, so don't export them

/*

func IsNotSignedError(err error) bool          { return errors.Is(err, notSignedError) }
func IsMalformedSignatureError(err error) bool { return errors.Is(err, malformedSignatureError) }
func IsUnknownKeyError(err error) bool         { return errors.Is(err, unknownKeyError) }
func IsAlgMismatchError(err error) bool        { return errors.Is(err, algMismatchError) }
func IsSignatureExpiredError(err error) bool   { return errors.Is(err, signatureExpiredError) }
func IsInvalidSignatureError(err error) bool   { return errors.Is(err, invalidSignatureError) }

*/

func verifyRsaPssSha512(pk *rsa.PublicKey) verHolder {
	return verHolder{
		alg: "rsa-pss-sha512",
		verifier: func() verImpl {
			h := sha512.New()

			return verImpl{
				w: h,
				verify: func(s []byte) error {
					b := h.Sum(nil)

					return rsa.VerifyPSS(pk, crypto.SHA512, b, s, nil)
				},
			}
		},
	}
}

func verifyEccP256(pk *ecdsa.PublicKey) verHolder {
	return verHolder{
		alg: "ecdsa-p256-sha256",
		verifier: func() verImpl {
			h := sha256.New()

			return verImpl{
				w: h,
				verify: func(s []byte) error {
					b := h.Sum(nil)

					if !ecdsa.VerifyASN1(pk, b, s) {
						return errInvalidSignature
					}

					return nil
				},
			}
		},
	}
}

func verifyHmacSha256(secret []byte) verHolder {
	return verHolder{
		alg: "hmac-sha256",
		verifier: func() verImpl {
			h := hmac.New(sha256.New, secret)

			return verImpl{
				w: h,
				verify: func(in []byte) error {
					if !hmac.Equal(in, h.Sum(nil)) {
						return errInvalidSignature
					}
					return nil
				},
			}
		},
	}
}
