-- Add configurable display ordering for models.

ALTER TABLE models
    ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 100;

COMMENT ON COLUMN models.sort_order IS 'Display order in model selection lists; lower values appear first';
