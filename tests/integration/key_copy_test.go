package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrivilegedKeyRecoveryRemoved(t *testing.T) {
	// A legacy deployment secret must never re-enable recovery or ciphertext.
	t.Setenv("OLR_API_KEY_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	f := startPortal(t)
	operator, person := f.browser(t), f.browser(t)
	operator.login(t, "operator", []string{"admins"})
	owner := person.login(t, "reader", []string{"staff"})
	data := adminCall(t, person, "POST", "/portal/api/keys", map[string]string{"label": "one-time"}, 201)
	var issued struct{ Key string }
	if err := json.Unmarshal(data, &issued); err != nil || issued.Key == "" {
		t.Fatal("issue key", err)
	}
	detail := managedDetail(t, operator, owner)
	if len(detail.Keys) != 1 || detail.Keys[0].CopyAvailable {
		t.Fatal("recovery advertised")
	}
	id := detail.Keys[0].ID
	var hash []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT hash FROM api_keys WHERE id=$1`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(issued.Key))
	if !bytes.Equal(hash, digest[:]) {
		t.Fatal("hash missing")
	}
	var columns int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_name='api_keys' AND column_name IN ('key_ciphertext','key_encryption_id')`).Scan(&columns); err != nil || columns != 0 {
		t.Fatal("recoverable storage remains", columns, err)
	}
	path := "/portal/api/admin/users/" + owner + "/keys/" + id + "/copy"
	adminCall(t, person, "POST", path, map[string]any{}, 403)
	resp, _ := operator.call(t, "POST", path, `{}`, "https://evil.test")
	if resp.StatusCode != 403 {
		t.Fatal("origin bypass")
	}
	data = adminCall(t, operator, "POST", path, map[string]any{}, 410)
	if strings.Contains(string(data), issued.Key) {
		t.Fatal("recovery returned secret")
	}
	for _, path := range []string{"/portal/api/keys", "/portal/api/admin/users/" + owner} {
		client := operator
		if path == "/portal/api/keys" {
			client = person
		}
		data = adminCall(t, client, "GET", path, nil, 200)
		if strings.Contains(string(data), issued.Key) {
			t.Fatal("metadata returned secret")
		}
	}
}
