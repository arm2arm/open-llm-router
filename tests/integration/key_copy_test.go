package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
)

func TestAdminEncryptedKeyCopy(t *testing.T) {
	t.Setenv("OLR_API_KEY_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	f := startPortal(t)
	operator, person := f.browser(t), f.browser(t)
	operator.login(t, "operator", []string{"admins"})
	owner := person.login(t, "reader", []string{"staff"})
	created := adminCall(t, person, "POST", "/portal/api/keys", map[string]string{"label": "encrypted"}, 201)
	var issued struct{ Key string }
	if json.Unmarshal(created, &issued) != nil || issued.Key == "" {
		t.Fatal("key not issued")
	}
	metadata := managedDetail(t, operator, owner)
	if len(metadata.Keys) != 1 || !metadata.Keys[0].CopyAvailable {
		t.Fatal("copy unavailable for encrypted key")
	}
	id := metadata.Keys[0].ID
	var sealed, hash []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT key_ciphertext,hash FROM api_keys WHERE id=$1`, id).Scan(&sealed, &hash); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(issued.Key))
	if bytes.Contains(sealed, []byte(issued.Key)) || !bytes.Equal(hash, digest[:]) {
		t.Fatal("key not stored securely")
	}
	path := "/portal/api/admin/users/" + owner + "/keys/" + id + "/copy"
	adminCall(t, person, "POST", path, map[string]any{}, 403)
	response, _ := operator.call(t, "POST", path, `{}`, "https://evil.example")
	if response.StatusCode != 403 {
		t.Fatal("cross-origin copy accepted")
	}
	adminCall(t, operator, "POST", path, map[string]string{"extra": "field"}, 400)
	data := adminCall(t, operator, "POST", path, map[string]any{}, 200)
	var copied struct{ Key string }
	_ = json.Unmarshal(data, &copied)
	if copied.Key != issued.Key {
		t.Fatal("wrong copied key")
	}
	for _, endpoint := range []string{"/portal/api/keys", "/portal/api/admin/users/" + owner} {
		client := operator
		if endpoint == "/portal/api/keys" {
			client = person
		}
		body := adminCall(t, client, "GET", endpoint, nil, 200)
		if strings.Contains(string(body), issued.Key) || strings.Contains(string(body), "key_ciphertext") || strings.Contains(string(body), "key_encryption_id") {
			t.Fatal("secret material leaked in metadata")
		}
	}
	cipher, err := auth.LoadKeyCipher(func(string) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)) })
	if err != nil {
		t.Fatal(err)
	}
	store := auth.Admin{Pool: f.pool, KeyCipher: cipher}
	plain, err := store.CopyKey(t.Context(), owner, id)
	if err != nil || plain != issued.Key {
		t.Fatal("copy after restart failed")
	}
	wrong, _ := auth.LoadKeyCipher(func(string) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)) })
	if _, err := (auth.Admin{Pool: f.pool, KeyCipher: wrong}).CopyKey(t.Context(), owner, id); !errors.Is(err, auth.ErrKeyUnavailable) {
		t.Fatal("wrong master key accepted")
	}
	other, err := store.AddUser(t.Context(), "other", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CopyKey(t.Context(), other, id); !errors.Is(err, auth.ErrNotFound) {
		t.Fatal("owner scope ignored")
	}
	legacy, err := (auth.Admin{Pool: f.pool}).CreateKey(t.Context(), owner, "legacy", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	legacyHash := sha256.Sum256([]byte(legacy))
	var legacyID string
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM api_keys WHERE hash=$1`, legacyHash[:]).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	adminCall(t, operator, "POST", "/portal/api/admin/users/"+owner+"/keys/"+legacyID+"/copy", map[string]any{}, 409)
	if err := store.SetEnabled(t.Context(), owner, false); err != nil {
		t.Fatal(err)
	}
	adminCall(t, operator, "POST", path, map[string]any{}, 404)
	if err := store.SetEnabled(t.Context(), owner, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE api_keys SET expires=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	adminCall(t, operator, "POST", path, map[string]any{}, 404)
	if _, err := f.pool.Exec(t.Context(), `UPDATE api_keys SET expires=clock_timestamp()+interval '1 hour',revoked=true WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	adminCall(t, operator, "POST", path, map[string]any{}, 404)
}
