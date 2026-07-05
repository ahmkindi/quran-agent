// Package turncred mints ephemeral TURN credentials for coturn's
// `use-auth-secret` mode (the "TURN REST API" scheme,
// draft-uberti-rtcweb-turn-rest): the username is "<expiryUnix>:<id>" and the
// credential is base64(HMAC-SHA1(username, shared-secret)). coturn validates
// the HMAC with the same static-auth-secret and rejects expired usernames, so
// the web server can hand short-lived TURN access to browsers without ever
// exposing the secret.
package turncred

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

// New returns an ephemeral (username, credential) pair valid for ttl.
func New(secret, id string, ttl time.Duration) (username, credential string) {
	expiry := time.Now().Add(ttl).Unix()
	username = fmt.Sprintf("%d:%s", expiry, id)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	credential = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return username, credential
}
