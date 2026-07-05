package turncred

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNew verifies the coturn use-auth-secret contract: username is
// "<futureUnix>:<id>" and credential is base64(HMAC-SHA1(username, secret)).
func TestNew(t *testing.T) {
	const secret, id = "test-shared-secret", "sess42"
	user, cred := New(secret, id, 10*time.Minute)

	expiryStr, gotID, ok := strings.Cut(user, ":")
	if !ok || gotID != id {
		t.Fatalf("username %q: want <expiry>:%s", user, id)
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		t.Fatalf("expiry %q not unix seconds: %v", expiryStr, err)
	}
	now := time.Now().Unix()
	if expiry <= now || expiry > now+11*60 {
		t.Fatalf("expiry %d not within (now, now+11m]", expiry)
	}

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(user))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); cred != want {
		t.Fatalf("credential = %q, want %q", cred, want)
	}
}
