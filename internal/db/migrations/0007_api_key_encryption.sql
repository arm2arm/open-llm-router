-- Existing hashes remain valid; only newly issued keys can be recovered.
ALTER TABLE api_keys ADD COLUMN key_ciphertext bytea;
ALTER TABLE api_keys ADD COLUMN key_encryption_id text;
ALTER TABLE api_keys ADD CONSTRAINT api_key_cipher_pair CHECK
    ((key_ciphertext IS NULL) = (key_encryption_id IS NULL));
