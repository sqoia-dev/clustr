-- Migration 013: add built_for_roles and build_method columns to base_images.
-- built_for_roles: JSON array of HPC role IDs selected at ISO build time.
-- build_method: how the image was created ("pull", "import", "capture", "iso").

ALTER TABLE base_images ADD COLUMN built_for_roles TEXT NOT NULL DEFAULT '[]';
ALTER TABLE base_images ADD COLUMN build_method TEXT NOT NULL DEFAULT '';
