-- Migration: 000087_user_name (rollback)

ALTER TABLE users DROP COLUMN IF EXISTS name;
