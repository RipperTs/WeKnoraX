-- Migration: 000087_user_name
-- Adds an optional display name to user accounts.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS name VARCHAR(100) NOT NULL DEFAULT '';

COMMENT ON COLUMN users.name IS 'Name of the user';
