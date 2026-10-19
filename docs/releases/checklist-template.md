# vX.Y.Z Release Checklist

Copy this file to `docs/releases/vX.Y.Z-checklist.md` and fill in every item with its evidence. See [Release Process](../howto/releaseProcess.md).

- **Release:** vX.Y.Z
- **Beta candidate:** vX.Y.Z-beta-N (SHA: )
- **Consensus-affecting scope:** Yes / No

## Operational Readiness

| Item | Evidence |
|---|---|
| Mainnet nodes on the beta for 24 hours or more, following the chain tip | |
| Testnet node on the beta for 24 hours or more, following the chain tip | |
| TeraTestNet node on the beta for 24 hours or more, following the chain tip | |
| Sync from zero on the beta | |
| Upgrade or restart during sync on the beta | |
| Seed-to-tip time on the reference hardware, measured again if the seeder or the snapshot export format changed since the last release. Or N/A and the reason. | |

## QA and Testing

| Item | Evidence |
|---|---|
| `teranode_main_tests.yaml` run at the beta SHA (unit, smoke, pruner) | |
| Consensus package results at the beta SHA (`services/blockvalidation`, `services/validator`, `services/subtreevalidation`, `services/blockassembly`, `stores/utxo`) | |

## Consensus Parity

| Item | Evidence |
|---|---|
| node-validation run against vX.Y.Z in full mode: report, verdict, tool version, mode. Or N/A and the reason. | |

## Security

| Item | Evidence |
|---|---|
| Security review or scan result for this release, or "none in scope" and the reason | |

## Architecture

| Item | Evidence |
|---|---|
| Architecture reviews or decision records that affect this release, or "none" | |

## Open Findings

| Item | Evidence |
|---|---|
| Open bug-labelled issues that affect the build | |
| Open Critical or High findings from any source, each closed or waived | |

### Waivers

| Finding | Owner | Date | Reason the risk is accepted | Close by |
|---|---|---|---|---|

## Miner Feedback Register

| Item | Evidence |
|---|---|
| Open items | |
| Items closed since the last release | |
| Where the register is | |

## Go/No-Go Decision

- **Criteria:** every item above has evidence, and every open Critical or High finding is closed or waived.
- **Open items and their disposition:**
- **Approver (maintainer):**
- **Date:**

**Decision:** No-Go
