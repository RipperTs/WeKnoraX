-- Mirrors versioned migration 000087_user_name.

ALTER TABLE users ADD COLUMN name VARCHAR(100) NOT NULL DEFAULT '';
