# 🌐 P2P Service

## Index

1. [Description](#1-description)
2. [Functionality](#2-functionality)
    - [2.1. Creating, initializing and starting a new P2P Server](#21-creating-initializing-and-starting-a-new-p2p-server)
    - [2.1.1. Creating a New P2P Server](#211-creating-a-new-p2p-server)
    - [2.1.2. Initializing the P2P Server](#212-initializing-the-p2p-server)
    - [2.1.3. Starting the P2P Server](#213-starting-the-p2p-server)
    - [2.2. Peer Discovery and Connection](#22-peer-discovery-and-connection)
    - [2.3. Best Block Messages](#23-best-block-messages)
    - [2.4. Blockchain Messages](#24-blockchain-messages)
    - [2.5. TX Validator Messages](#25-tx-validator-messages)
    - [2.6. Websocket notifications](#26-websocket-notifications)
    - [2.7. Ban Management System](#27-ban-management-system)
        - [2.7.1. Ban List Management](#271-ban-list-management)
        - [2.7.2. Ban Operations](#272-ban-operations)
        - [2.7.3. Ban Event Handling](#273-ban-event-handling)
        - [2.7.4. Configuration](#274-configuration)
    - [3. Technology](#3-technology)
    - [4. Data Model](#4-data-model)
    - [5. Directory Structure and Main Files](#5-directory-structure-and-main-files)
    - [6. How to run](#6-how-to-run)
    - [7. Configuration options (settings flags)](#7-configuration-options-settings-flags)
    - [8. Other Resources](#8-other-resources)

## 1. Description

The p2p package implements a peer-to-peer (P2P) server using `libp2p` (`github.com/libp2p/go-libp2p`, `https://libp2p.io`), a modular network stack that allows for direct peer-to-peer communication. The implementation follows an interface-based design pattern using the public `github.com/bsv-blockchain/go-p2p` package, with `p2p.NodeI` interface abstracting the concrete P2P node implementation to allow for better testability and modularity. This public package enables external developers to create custom P2P implementations that integrate with Teranode.

The p2p service allows peers to subscribe and receive blockchain notifications, effectively allowing nodes to receive notifications about new blocks and subtrees in the network.

The p2p peers are part of a private network. This private network is managed by the p2p bootstrap service, which is responsible for bootstrapping the network and managing the network topology.

1. **Initialization and Configuration**:

    - The `Server` struct holds essential information for the P2P server, such as hosts, topics, subscriptions, clients for blockchain and validation, and logger for logging activities.
    - The `NewServer` function creates a new instance of the P2P server, sets up the host with a private key, and defines various topic names for message publishing and subscribing.
    - The `Init` function configures the server with necessary clients and settings.

2. **Message Types**:

    - There are several message types (`BestBlockMessage`, `MiningOnMessage`, `BlockMessage`, `SubtreeMessage`, `RejectedTxMessage`) used for communicating different types of data over the network.
    - The `BlockMessage` includes a `Header` field containing the raw block header in hexadecimal format, allowing peers to validate block properties without downloading the full block.

3. **Networking and Communication**:

    - The server uses `libp2p` for network communication. It sets up a host with given IP and port and handles different topics for publishing and subscribing to messages.
    - The server uses Kademlia for peer discovery, implementing both standard and private DHT (Distributed Hash Table) modes based on configuration.
    - The server uses GossipSub for PubSub messaging, with automated topic subscription and management.
    - Peers identify themselves using a user agent string in format `teranode/bitcoin/{version}` where the version is dynamically determined at build time from Git tags (e.g., "v1.2.3") or generated as a pseudo-version (e.g., "v0.0.0-20250731141601-18714b9").
    - `handleBlockchainMessage`, `handleBestBlockTopic`, `handleBlockTopic`, `handleSubtreeTopic`, and `handleMiningOnTopic` are functions to handle incoming messages for different topics.
    - `sendBestBlockMessage` and `sendPeerMessage` are used for sending messages to peers in the network.
    - The implementation includes error handling and retry mechanisms for peer connections to enhance network resilience.

4. **Peer Discovery and Connection**:

    - `discoverPeers` function is responsible for discovering peers in the network and attempting to establish connections with them.
    - The system implements an intelligent retry mechanism that tracks failed connection attempts and manages reconnection policies based on error types.
    - A dedicated mechanism for connecting to static peers runs in a separate goroutine, ensuring that mission-critical peers are always connected when available.

5. **HTTP Server and WebSockets**:

    - An HTTP server is started using `echo`, a Go web framework, providing routes for health checks and WebSocket connections.
    - `HandleWebSocket` is used to handle incoming WebSocket connections and manage the communication with connected clients.

6. **Subscription Listeners**:

    - The server listens for notifications from the blockchain and the tx validator services. It subscribes to these services and handles incoming notifications.

7. **Publish-Subscribe Mechanism**:

    - The server uses the publish-subscribe model over `libp2p` pubsub for message dissemination. It joins topics and subscribes to them to receive and broadcast messages related to blockchain events.

8. **Private Key Management**:

    - The server generates or reads a private key for secure communication (peerId and encryption) in the P2P network.
    - The generated private key is persisted.

> **Note**: For information about how the P2P service is initialized during daemon startup and how it interacts with other services, see the [Teranode Daemon Reference](../../references/teranodeDaemonReference.md#service-initialization-flow).

![P2P_System_Container_Diagram.png](img/P2P_System_Container_Diagram.png)

In the diagram above:

- The node P2P service subscribes and listens to Blockchain and Tx Validator service notifications. When blocks, subtrees or rejected transactions are detected, the P2P service publishes these messages to the network.
- When the P2P service receives a message from the network (i.e. a Blockchain message from another node), it forwards it to the node Block Validation Service for processing.

In more detail:

![P2P_System_Component_Diagram.png](img/P2P_System_Component_Diagram.png)

The following diagram provides a deeper level of detail into the P2P Service's internal components and their interactions:

![p2p_detailed_component.svg](img/plantuml/p2p/p2p_detailed_component.svg)

## 2. Functionality

### 2.1. Creating, initializing and starting a new P2P Server

![p2p_create_init_start.svg](img/plantuml/p2p/p2p_create_init_start.svg)

### 2.1.1. Creating a New P2P Server

The startup process of the node involves the `main.go` file calling the `p2p.NewServer` function from the P2P package (`services/p2p/Server.go`). This function is tasked with creating a new P2P server instance.

1. **Private Key Management**:

    - The server tries to read an existing private key from the blockchain store through the `readPrivateKey()` function:

        - `readPrivateKey` calls `blockchainClient.GetState(ctx, "p2p.privateKey")` to retrieve the serialized key data
        - If found, it deserializes the key using `crypto.UnmarshalPrivateKey()`
    - If no key is specified in the configuration and no key exists in the blockchain store, it automatically generates and stores a new one using the `generateAndStorePrivateKey()` function:

        - `generateAndStorePrivateKey` creates a new Ed25519 key pair using `crypto.GenerateEd25519Key()`
        - It serializes the private key with `crypto.MarshalPrivateKey()`
        - It stores the serialized key in the blockchain store using `blockchainClient.SetState(ctx, "p2p.privateKey", privBytes)`
        - The generated key is automatically persisted to ensure the node maintains the same peer ID across restarts

    - This blockchain store persistence mechanism works through gRPC calls to the blockchain service
    - Using the blockchain store ensures that the P2P private key (and therefore node identity) persists even if the container is destroyed, maintaining consistent peer relationships in the network

    ![p2p_private_key_persistence.svg](img/plantuml/p2p/p2p_private_key_persistence.svg)

2. **Configuration Retrieval and Topic Registration**:

    - Retrieves required configuration settings like `p2p_listen_addresses` and `p2p_port`.
    - It registers specific topic names derived from the configuration, such as `p2p_block_topic`, `p2p_subtree_topic`, `p2p_bestblock_topic`, `p2p_mining_on_topic`, and `p2p_rejected_tx_topic`.

3. **P2P Node Initialization**:

    - Initializes a libp2p node (host) using the specified IP, port, and private key, which manages node communications and connections.

### 2.1.2. Initializing the P2P Server

The P2P server's `Init` function is in charge of server setup:

1. **Blockchain Client Initialization**:

    - Creates a new blockchain client with `blockchain.NewClient(ctx, s.logger, "source")`.

2. **Asset HTTP Address Configuration**:

    - Retrieves the Asset HTTP Address URL from the configuration using `gocore.Config().GetURL("asset_httpAddress")`.

3. **Block Validation Client Initialization**:

    - Sets up a block validation client with `blockvalidation.NewClient(ctx)`.

4. **Validator Client Initialization**:

    - Initializes a transaction validator client with `validator.NewClient(ctx, s.logger)`.

### 2.1.3. Starting the P2P Server

The `Start` function is responsible for commencing the P2P service:

1. **HTTP Server Setup**:

    - Utilizes the Echo framework to set up an HTTP server.

2. **HTTP Endpoints**:

    - Sets up a health check endpoint (`/health`) that responds with "OK".
    - Adds a WebSocket endpoint (`/ws`) for real-time communication via `s.HandleWebSocket`.

3. **Start HTTP Server**:

    - Initiates the HTTP server using a goroutine, which is executed via `s.StartHttp`.

4. **PubSub Topics Setup**:

    - Initializes the GossipSub system for pub-sub messaging.
    - Subscribes to various topics defined in the configuration.

5. **Peer-to-Peer Host Configuration**:

    - Assigns a stream handler to the P2P host to handle blockchain-related messages.

6. **Topic Handlers**:

    - Initiates goroutines for message handling on designated topics.

7. **Subscription Listeners**:

    - Begins subscription listeners for blockchain and validator services to manage incoming notifications about blocks and transactions.

8. **Best Block Message Broadcast**:

    - Sends a best block message to the network to solicit the current best block hash from peers.

Once these steps are completed, the server is ready to accept peer connections, handle messages, and monitor network activity.

### 2.2. Peer Discovery and Connection

In the previous section, the P2P Service created a `P2PClient as part of the initialization phase. This P2PClient is responsible for joining the network. The P2PClient utilizes a libp2p host and a Distributed Hash Table (DHT) for peer discovery and connection based on shared topics. This mechanism enables the client to become part of a decentralized network, facilitating communication and resource sharing among peers.

![p2p_peer_discovery.svg](img/plantuml/p2p/p2p_peer_discovery.svg)

1. **Initialization of DHT and libp2p Host**:

    - The `P2PClient` struct, upon invocation of its `Start` method, initializes the DHT using either `initDHT` or `initPrivateDHT` methods depending on the configuration. This step sets up the DHT for the libp2p host (`s.host`), which allows for peer discovery and content routing within the P2P network. The DHT is bootstrapped with default or configured peers to integrate the client into the existing network.

2. **Setting Up Routing Discovery**:

    - Once the DHT is initialized, `P2PClient.Start` sets up `routingDiscovery` with the created DHT instance. This discovery service is responsible for locating peers within the network and advertising the client's own presence.
    - The DHT implementation supports two modes:

        - Standard mode (`initDHT`): Uses the public IPFS bootstrap nodes for initial discovery
        - Private mode (`initPrivateDHT`): Creates an isolated private network with custom bootstrap nodes, providing enhanced security for enterprise deployments

3. **Advertising and Searching for Peers**:

    - The node then advertises itself for the configured topics and looks for peers associated with these topics. This is conducted through the `discoverPeers` method, which iterates over the topic names and uses the routing discovery to advertise and find peers interested in the same topics.

4. **Connecting to Discovered and Static Peers**:

    - The `discoverPeers` method includes sophisticated filtering and error handling mechanisms:

        - `shouldSkipPeer`: Determines if connection attempts should be skipped based on various criteria
        - `shouldSkipBasedOnErrors`: Manages retry logic for previously failed connections
        - `shouldSkipNoGoodAddresses`: Special handling for peers with address resolution issues

    - The `connectToStaticPeers` method runs in a dedicated goroutine to periodically attempt connections with a predefined list of peers (static peers), ensuring critical network infrastructure remains connected even after temporary failures.
    - Connection errors are carefully tracked to avoid network congestion from repeated failed connection attempts.

5. **Integration with P2P Network**:

    - The capabilities of the DHT for discovery and topic-based advertising enables the node to seamlessly integrate into the Teranode P2P network.

### 2.3. Best Block Messages

![p2p_handle_blockchain_messages.svg](img/plantuml/p2p/p2p_handle_blockchain_messages.svg)

1. **Node (Peer 1) Starts**:

    - The server's `Start()` method is invoked. Within `Start()`, `s.sendBestBlockMessage(ctx)` is called to send the best block message.
    - This message is published to a topic using `s.topics[bestBlockTopicName].Publish(ctx, msgBytes)`.

2. **Peer 2 Receives the Best Block Topic Message**:

    - Peer 2's server handles the best block topic through `s.handleBestBlockTopic(ctx)`.
    - A peer message is sent using `s.sendPeerMessage(ctx, peer, msgBytes)`. A new stream to the Peer 1 LibP2P host is established with `s.host.NewStream(ctx, peer.ID, protocol.ID(s.bitcoinProtocolId))`.

3. **Node (Peer 1) Receives the Stream Response from Peer 2**:

    - The LibP2P host sends the response stream to Node (Peer 1).
    - Node (Peer 1) handles the blockchain message using `s.handleBlockchainMessage(ctx, stream)`.
    - The message received is logged with `s.logger.Debugf("Received block topic message: %s", string(buf))` (at the time of writing, the `handleBlockchainMessage` function simply logs block topic messages).

### 2.4. Blockchain Messages

When a node creates a new subtree, or finds a new block hashing solution, it will broadcast this information to the network. This is done by publishing a message to the relevant topic. The message is then received by all peers subscribed to that topic. Listening peers can then feed relevant messages to their own Block Validation Service.

![p2p_blockchain_subscription.svg](img/plantuml/p2p/p2p_blockchain_subscription.svg)

1. **Blockchain Subscription**:

    - The server subscribes to the blockchain service using `s.blockchainClient.Subscribe(ctx, blockchain.SubscriptionType_Blockchain)`.
    - The server listens for blockchain notifications (`Block`, `Subtree` or `MiningOn` notifications) using `s.blockchainSubscriptionListener(ctx)`.

2. **New Block Notification**:

    - Node 1 listens for blockchain notifications.
    - If a new block notification is detected, it publishes the block message to the PubSub System.
    - The block message includes the block hash, height, data hub URL, peer ID, and the raw block header in hexadecimal format.
    - The PubSub System then delivers this message to Node 2.
    - Node 2 receives the message on the block topic, can quickly validate the block header, then **submits the block message to its own Block Validation Service**, and notifies the block message on its notification channel.
    - Note that the Block Validation Service might be configured to either receive gRPC notifications or listen to a Kafka producer. In the diagram above, the gRPC method is described. Please check the [Block Validation Service](blockValidation.md) documentation for more details

3. **New Mined Block Notification**:

    - Node 1 listens for blockchain notifications.
    - If a new mined block notification is detected, it publishes the mining on message to the PubSub System.
    - The PubSub System delivers this message to Node 2.
    - Node 2 receives the mining message on the mining topic and notifies the "mining on" message on its notification channel.

4. **New Subtree Notification**:

    - Node 1 listens for blockchain notifications.
    - If a new subtree notification is detected, it publishes the subtree message to the PubSub System.
    - The PubSub System delivers this message to Node 2.
    - Node 2 receives the subtree message on the subtree topic, **submits the subtree message to its own Subtree Validation Service**, and notifies the subtree message on its notification channel.
        - Note that the Subtree Validation Service might be configured to either receive gRPC notifications or listen to a Kafka producer. In the diagram above, the gRPC method is described. Please check the [Subtree Validation Service](subtreeValidation.md)  documentation for more details

### 2.5. TX Validator Messages

Nodes will broadcast rejected transaction notifications to the network. This is done by publishing a message to the relevant topic. The message is then received by all peers subscribed to that topic.

![p2p_tx_validator_messages.svg](img/plantuml/p2p/p2p_tx_validator_messages.svg)

- The Node 1 listens for validator subscription events.
- When a new rejected transaction notification is detected, the Node 1 publishes this message to the PubSub System using the topic name `rejectedTxTopicName`, forwarding it to any subscribers of the `rejectedTxTopicName` topic.

Note that the P2P service can only subscribe to these notifications if and when the TX Validator Service is available in the node. The service uses the `useLocalValidator` setting to determine whether a local validator or a validator service is in scope. If no TX validator runs in the node, the P2P will not attempt to subscribe.

### 2.6. Websocket notifications

All notifications collected from the Block and Validator listeners are sent over to the Websocket clients. The process can be seen below:

![p2p_websocket_activity_diagram.svg](img/plantuml/p2p/p2p_websocket_activity_diagram.svg)

- WebSocket Request Handling:

  - An HTTP request is upgraded to a WebSocket connection. A new client channel is associated to this Websocket client.
  - Data is sent over the WebSocket, using its dedicated client channel.
  - If there's an error in sending data, the channel is removed from the `clientChannels`.

- The server listens for various types of events in a concurrent process:

  - The server tracks all active client channels (`clientChannels`).
  - When a new client connects, it is added to the `clientChannels`.
  - If a client disconnects, it is removed from `clientChannels`.
  - Periodically, we ping all connected clients. Any error would have the client removed from the list of clients.
  - When a notification is received (from the block validation or transaction listeners described in the previous sections), it is sent to all connected clients.

As a sequence:

![p2p_websocket_sequence_diagram.svg](img/plantuml/p2p/p2p_websocket_sequence_diagram.svg)

1. A client requests a WebSocket connection to the server. The new client is added to the `newClientCh` queue, which then adds the client to the active client channels.
2. The server enters a loop for WebSocket communication, where it can either receive new notifications or pings.
3. For each new notification or ping, the server dispatches this data to all client channels.
4. If there's a WebSocket error or the client disconnects, the client is added to the `deadClientCh` queue, which leads to its removal from the active client channels.

### 2.7. Ban Management System

The P2P service includes a ban management system that allows nodes to maintain a list of banned peers and handle ban-related events across the network.

![p2p_ban_system.svg](img/plantuml/p2p/p2p_ban_system.svg)

#### 2.7.1. Ban List Management

The ban system consists of two main components:

1. **BanList**: A thread-safe data structure that maintains banned IP addresses and subnets
2. **BanChan**: A channel that broadcasts ban-related events to system components

The ban list supports:

- Individual IP addresses
- Entire subnets using CIDR notation
- Temporary bans with expiration times
- Persistent storage of bans in a database

#### 2.7.2. Ban Operations

While the banList is maintained by the P2P service, the ban operations are managed by the RPC Server. The RPC server receives request to add / remove peers, notifying the P2P service to update the ban list.

#### 2.7.3. Ban Event Handling

When a ban event occurs:

1. The ban is added to the persistent storage
2. A ban event is broadcast through the ban channel
3. The P2P service checks current connections against the ban
4. Any matching connections are terminated
5. Future connection attempts from banned IPs are rejected

#### 2.7.4. Configuration

Ban-related settings in the configuration:

- `banlist_db`: Database connection string for ban storage
- `ban_default_duration`: Default duration for bans (24 hours if not specified)
- `ban_max_entries`: Maximum number of banned entries to maintain

## 3. Technology

1. **Go Programming Language**:

    - The entire package is written in Go (Golang).

2. **libp2p**:

    - A modular network framework that allows peers to communicate directly with each other. It's widely used in decentralized systems for handling various network protocols, peer discovery, transport, encryption, and stream multiplexing.

3. **pubsub (go-libp2p-pubsub)**:

    - A library for publish-subscribe functionality in `libp2p`. It's used for messaging between nodes in a decentralized network, supporting various messaging patterns like broadcasting.

4. **crypto (go-libp2p-core/crypto)**:

    - This library provides cryptographic functions for `libp2p`, including key generation, marshaling, and unmarshaling. It's crucial for maintaining secure communication channels in the P2P network.

5. **Echo (labstack/echo/v4)**:

    - A high-performance, extensible web framework for Go.

6. **WebSockets**:

    - A communication protocol providing full-duplex channels over a single TCP connection. Used here for real-time, two-way interaction between the server and clients.

7. **JSON (JavaScript Object Notation)**:

    - A lightweight data-interchange format, used here for serializing and transmitting structured data over the network (like the various message types).

8. **Distributed Hash Table (DHT) for Peer Discovery**:

    - A decentralized method of discovering peers in the network. It allows a node to efficiently find other nodes in the P2P network.

9. **HTTP/HTTPS Protocols**:

    - Used for setting up the web server and handling requests. The server can handle both HTTP and HTTPS requests.

10. **Middleware & Logging (middleware, ulogger)**:

    - Used for handling common tasks across requests (like logging, error handling, CORS settings) in the Echo web framework.

11. **gocore (ordishs/gocore)**:

    - A utility library for configuration management and other core functionalities.

12. **go-p2p (github.com/bsv-blockchain/go-p2p)**:

    - Public P2P networking package that provides the core P2P node implementation
    - Offers standardized interfaces for P2P communication in Bitcoin SV applications
    - Enables external developers to build compatible P2P solutions

13. **Environmental Configuration**:

    - Configuration management using environment variables, required for setting up network parameters, topic names, etc.

## 4. Data Model

- [Block Data Model](../datamodel/block_data_model.md): Contain lists of subtree identifiers.
- [Subtree Data Model](../datamodel/subtree_data_model.md): Contain lists of transaction IDs and their Merkle root.

Within the P2P service, notifications are sent to the Websocket clients using the following data model:

- Block notifications:

```go
   s.notificationCh <- &notificationMsg{
    Timestamp: time.Now().UTC(),
    Type:      "block",
    Hash:      blockMessage.Hash,
    BaseURL:   blockMessage.DataHubUrl,
    PeerId:    blockMessage.PeerId,
   }

```

- Subtree notifications:

```go
   s.notificationCh <- &notificationMsg{
    Type:    "subtree",
    Hash:    subtreeMessage.Hash,
    BaseURL: subtreeMessage.DataHubUrl,
    PeerId:  subtreeMessage.PeerId,
   }
```

- "MiningOn" notifications:

```go
   s.notificationCh <- &notificationMsg{
    Timestamp:    time.Now().UTC(),
    Type:         "mining_on",
    Hash:         miningOnMessage.Hash,
    BaseURL:      miningOnMessage.DataHubUrl,
    PeerId:       miningOnMessage.PeerId,
    PreviousHash: miningOnMessage.PreviousHash,
    Height:       miningOnMessage.Height,
    Miner:        miningOnMessage.Miner,
    SizeInBytes:  miningOnMessage.SizeInBytes,
    TxCount:      miningOnMessage.TxCount,
   }
```

## 5. Directory Structure and Main Files

```text
./services/p2p
│
├── HandleWebsocket.go   - Manages WebSocket connections and communications.
├── Server.go            - Main server logic for the P2P service, including network handling and peer interactions.
├── client.html          - A client-side HTML file for testing or interacting with the WebSocket server.
└── dht.go               - Implements the Distributed Hash Table (DHT) functionality for the P2P network.
```

## 6. How to run

To run the P2P Service locally, you can execute the following command:

```shell
SETTINGS_CONTEXT=dev.[YOUR_USERNAME] go run -P2P=1
```

Please refer to the [Locally Running Services Documentation](../../howto/locallyRunningServices.md) document for more information on running the P2P Service locally.

## 7. Configuration options (settings flags)

For comprehensive configuration documentation including all settings, defaults, and interactions, see the [p2p Settings Reference](../../references/settings/services/p2p_settings.md).

## 8. Other Resources

[P2P Reference](../../references/services/p2p_reference.md)
