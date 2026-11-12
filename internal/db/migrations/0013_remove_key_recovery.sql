-- Authentication uses hashes; destroy historical recoverable ciphertext.
ALTER TABLE api_keys DROP COLUMN key_ciphertext, DROP COLUMN key_encryption_id;
