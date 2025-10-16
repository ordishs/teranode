# Comprehensive Settings Reference

This document provides a complete reference for all Teranode configuration settings, organized by component.

## Table of Contents

1. [Overview](#overview)
2. [General Configuration](#general-configuration)
3. [Services](#services)

## Overview

All Teranode services accept settings through a centralized Settings object that allows local and remote servers to have their own specific configuration.

For general information on how the configuration system works, see the [Settings Overview](settings.md).

For deployment-specific information, see:

- [Developer Setup](../tutorials/developers/developerSetup.md)
- [Docker Configuration](../howto/miners/docker/minersHowToConfigureTheNode.md)
- [Kubernetes Configuration](../howto/miners/kubernetes/minersHowToConfigureTheNode.md)

---

## General Configuration

### Configuration Files

Settings are stored in two files:

- `settings.conf`: Global settings with sensible defaults for all environments
- `settings_local.conf`: Developer-specific and deployment-specific overrides (not in source control)

### Configuration System

The configuration system uses a layered approach with the following priority:

1. `SETTING_NAME.context_name`: Context-specific override (highest priority)
2. `SETTING_NAME.base`: General override
3. `SETTING_NAME`: Base setting (lowest priority)

### Environment Variables

Most settings can be configured via environment variables using the pattern:

```text
TERANODE_<SERVICE>_<SETTING_NAME>
```

---

## Services

For detailed service-specific configuration documentation, see:

- **[Alert Service](settings/services/alert_settings.md)** - Bitcoin SV alert system configuration
- **[Asset Server](settings/services/asset_settings.md)** - HTTP/WebSocket interface configuration
- **[Block Assembly](settings/services/blockassembly_settings.md)** - Block assembly service configuration
- **[Blockchain](settings/services/blockchain_settings.md)** - Blockchain state management configuration
- **[Block Persister](settings/services/blockpersister_settings.md)** - Block persistence configuration
- **[Block Validation](settings/services/blockvalidation_settings.md)** - Block validation configuration
- **[Legacy](settings/services/legacy_settings.md)** - Legacy Bitcoin protocol compatibility configuration
- **[P2P](settings/services/p2p_settings.md)** - Peer-to-peer networking configuration
- **[Propagation](settings/services/propagation_settings.md)** - Transaction propagation configuration
- **[RPC](settings/services/rpc_settings.md)** - JSON-RPC server configuration
- **[Subtree Validation](settings/services/subtreevalidation_settings.md)** - Subtree validation configuration
- **[UTXO Persister](settings/services/utxopersister_settings.md)** - UTXO set persistence configuration
- **[Validator](settings/services/validator_settings.md)** - Transaction validation configuration
