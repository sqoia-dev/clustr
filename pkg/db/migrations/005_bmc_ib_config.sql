ALTER TABLE node_configs ADD COLUMN bmc_config TEXT NOT NULL DEFAULT '{}';
ALTER TABLE node_configs ADD COLUMN ib_config TEXT NOT NULL DEFAULT '[]';
