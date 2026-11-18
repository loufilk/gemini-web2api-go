# ChuanAPI upstream sync, 2026-11-17

Candidate: `4.29.0+chuanapi.models2.latency3`.
First-parent baseline: `eb857ec355290ef27506451a251fe9f3fb2e4539`.
Upstream merged: `ce95dda9f6848ed113e9b6b616183a1c55db35c5` (v4.29.0).

## Imported behavior

- Hydrogen uses a signed-in account, not a blanket paid-account requirement. Actual eligibility remains account-catalog based.
- Import upstream mode-8 request fields, including `inner[49]=40`. Upstream reports this prevents hanging but can disable High thinking. This merge does not independently prove account behavior or faster generation.
- Retire both 3.1 Pro IDs before upstream submission, remove them from static and discovered public catalogs, and migrate a retired default model at startup without discarding other configuration.
- Import upstream 3.6 Flash strict-model recognition of Hydrogen and updated model descriptions.

## Intentional fork differences

- Argon and Carbon remain distinct identities. Upstream accepts them interchangeably; this fork does not. An account whose catalog exposes Carbon alone cannot satisfy an Argon request.
- Old 3.7/3.8 Flash input aliases remain canonicalized to Hydrogen and are hidden from advertised catalogs.
- Mandatory identity checks remain independent of `strict_model`; unknown/mismatched identity does not become successful premium output.
- Preserve per-account routing, stage latency metrics, caller cancellation, no blind replay after uncertain submission, stateless Responses and existing admin/media safeguards.

## Validation and rollout boundary

Local checks use synthetic fixtures, not real Google accounts. Successful local tests do not verify Hydrogen High, Carbon latency, upstream availability or production recovery. Keep New API channel #157 and production unchanged until a separately authorized isolated smoke test and rollout. Use the previous verified custom-image digest for rollback, not upstream `latest`; see the existing model rollout and latency follow-up guides.
