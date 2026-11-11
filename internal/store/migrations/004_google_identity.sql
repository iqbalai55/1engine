ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE users ADD COLUMN google_subject text;
CREATE UNIQUE INDEX users_google_subject_idx ON users(google_subject) WHERE google_subject IS NOT NULL;
