package main

// heldStorageRateLabel is the held-snapshot storage rate as the CLI writes it.
//
// MIRRORS the orchestrator's authoritative `STORAGE_USD_PER_GIB_MONTH`
// (aquanode-backend/orchestrator/src/configs/storage-billing.config.ts), which
// is what actually prices the ledger row, and the console's and website's own
// mirrors of it. Nothing links them at build time, so they move in the same
// change or this CLI quotes a rate we do not charge.
//
// UNIT: per GiB (2^30) per 30-day month — the same unit `formatSetupSize`
// renders sizes in, so `displayed size x rate` matches the invoice.
//
// Taking a snapshot is free and a bucket you bring yourself is never billed;
// only HOLDING one in Aquanode's managed bucket bills, metered per second.
const heldStorageRateLabel = "$0.02/GiB/mo"
