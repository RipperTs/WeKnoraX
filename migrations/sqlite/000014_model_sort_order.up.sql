-- Mirrors versioned migration 000088_model_sort_order.

ALTER TABLE models ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 100;
