-- 016_image_firmware: add firmware column to base_images.
-- Allowed values: 'uefi' (default, back-compat) and 'bios' (legacy BIOS/SeaBIOS).
-- Existing rows inherit 'uefi' via the column DEFAULT — no data migration needed.
ALTER TABLE base_images ADD COLUMN firmware TEXT NOT NULL DEFAULT 'uefi';
