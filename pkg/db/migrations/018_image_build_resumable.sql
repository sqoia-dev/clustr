-- 018_image_build_resumable: extend base_images with resumable-build fields.
--
-- resumable: when true, the build was interrupted cleanly and can be resumed
--   from resume_from_phase without starting over. Set by ReconcileStuckBuilds
--   and by the graceful-shutdown handler (SIGTERM path).
--
-- resume_from_phase: the last phase the build had reached when it was
--   interrupted. The resume endpoint re-enters the factory state machine at
--   this phase, reusing cached ISO and disk.raw where possible.

ALTER TABLE base_images ADD COLUMN resumable INTEGER NOT NULL DEFAULT 0;
ALTER TABLE base_images ADD COLUMN resume_from_phase TEXT NOT NULL DEFAULT '';
