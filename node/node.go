package node

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bytom/blockchain/account"
	"github.com/bytom/blockchain/asset"
	"github.com/bytom/blockchain/pin"
	"github.com/bytom/blockchain/pseudohsm"
	"github.com/bytom/blockchain/txdb"
	"github.com/bytom/consensus"
	"github.com/bytom/env"
	"github.com/bytom/errors"
	"github.com/bytom/net/http/reqid"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc/legacy"
	"github.com/bytom/types"
	"github.com/bytom/version"
	"github.com/kr/secureheader"
	"github.com/tendermint/tmlibs/log"

	bc "github.com/bytom/blockchain"
	cfg "github.com/bytom/config"
	bytomlog "github.com/bytom/log"
	p2p "github.com/bytom/p2p"
	rpccore "github.com/bytom/rpc/core"
	grpccore "github.com/bytom/rpc/grpc"
	rpcserver "github.com/bytom/rpc/lib/server"
	crypto "github.com/tendermint/go-crypto"
	wire "github.com/tendermint/go-wire"
	cmn "github.com/tendermint/tmlibs/common"
	dbm "github.com/tendermint/tmlibs/db"
	_ "net/http/pprof"
)

const (
	httpReadTimeout  = 2 * time.Minute
	httpWriteTimeout = time.Hour
)

type Node struct {
	cmn.BaseService

	// config
	config *cfg.Config

	// network
	privKey  crypto.PrivKeyEd25519 // local node's p2p key
	sw       *p2p.Switch           // p2p connections
	addrBook *p2p.AddrBook         // known peers

	// services
	evsw types.EventSwitch // pub/sub for services
	//    blockStore       *bc.MemStore
	blockStore   *txdb.Store
	bcReactor    *bc.BlockchainReactor
	accounts     *account.Manager
	assets       *asset.Registry
	rpcListeners []net.Listener // rpc servers
}

var (
	// config vars
	rootCAs       = env.String("ROOT_CA_CERTS", "") // file path
	splunkAddr    = os.Getenv("SPLUNKADDR")
	logFile       = os.Getenv("LOGFILE")
	logSize       = env.Int("LOGSIZE", 5e6) // 5MB
	logCount      = env.Int("LOGCOUNT", 9)
	logQueries    = env.Bool("LOG_QUERIES", false)
	maxDBConns    = env.Int("MAXDBCONNS", 10)           // set to 100 in prod
	rpsToken      = env.Int("RATELIMIT_TOKEN", 0)       // reqs/sec
	rpsRemoteAddr = env.Int("RATELIMIT_REMOTE_ADDR", 0) // reqs/sec
	indexTxs      = env.Bool("INDEX_TRANSACTIONS", true)
	home          = bc.HomeDirFromEnvironment()
	bootURL       = env.String("BOOTURL", "")
	// build vars; initialized by the linker
	buildTag    = "?"
	buildCommit = "?"
	buildDate   = "?"
	race        []interface{} // initialized in race.go
)

func NewNodeDefault(config *cfg.Config, logger log.Logger) *Node {
	return NewNode(config, logger)
}

func RedirectHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/" {
			http.Redirect(w, req, "/dashboard/", http.StatusFound)
			return
		}
		next.ServeHTTP(w, req)
	})
}

type waitHandler struct {
	h  http.Handler
	wg sync.WaitGroup
}

func (wh *waitHandler) Set(h http.Handler) {
	wh.h = h
	wh.wg.Done()
}

func (wh *waitHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	wh.wg.Wait()
	wh.h.ServeHTTP(w, req)
}

func rpcInit(h *bc.BlockchainReactor, config *cfg.Config) {
	// The waitHandler accepts incoming requests, but blocks until its underlying
	// handler is set, when the second phase is complete.
	var coreHandler waitHandler
	coreHandler.wg.Add(1)
	mux := http.NewServeMux()
	mux.Handle("/", &coreHandler)

	var handler http.Handler = mux
	//handler = core.AuthHandler(handler, raftDB, accessTokens, tlsConfig)
	handler = RedirectHandler(handler)
	handler = reqid.Handler(handler)

	secureheader.DefaultConfig.PermitClearLoopback = true
	secureheader.DefaultConfig.HTTPSRedirect = false
	secureheader.DefaultConfig.Next = handler

	server := &http.Server{
		// Note: we should not set TLSConfig here;
		// we took care of TLS with the listener in maybeUseTLS.
		Handler:      secureheader.DefaultConfig,
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
		// Disable HTTP/2 for now until the Go implementation is more stable.
		// https://github.com/golang/go/issues/16450
		// https://github.com/golang/go/issues/17071
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	listenAddr := env.String("LISTEN", config.ApiAddress)
	listener, _ := net.Listen("tcp", *listenAddr)

	// The `Serve` call has to happen in its own goroutine because
	// it's blocking and we need to proceed to the rest of the core setup after
	// we call it.
	go func() {
		err := server.Serve(listener)
		bytomlog.Fatalkv(context.Background(), bytomlog.KeyError, errors.Wrap(err, "Serve"))
	}()
	coreHandler.Set(h)
}

func NewNode(config *cfg.Config, logger log.Logger) *Node {
	ctx := context.Background()

	// Get store
	tx_db := dbm.NewDB("txdb", config.DBBackend, config.DBDir())
	store := txdb.NewStore(tx_db)

	privKey := crypto.GenPrivKeyEd25519()

	// Make event switch
	eventSwitch := types.NewEventSwitch()
	eventSwitch.SetLogger(logger.With("module", "types"))
	_, err := eventSwitch.Start()
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to start switch: %v", err))
	}

	p2pLogger := logger.With("module", "p2p")

	sw := p2p.NewSwitch(config.P2P)
	sw.SetLogger(p2pLogger)

	fastSync := config.FastSync

	genesisBlock := &legacy.Block{
		BlockHeader:  legacy.BlockHeader{},
		Transactions: []*legacy.Tx{},
	}
	genesisBlock.UnmarshalText(consensus.InitBlock())

	txPool := protocol.NewTxPool()
	chain, err := protocol.NewChain(ctx, genesisBlock.Hash(), store, txPool, nil)

	if store.Height() < 1 {
		if err := chain.AddBlock(nil, genesisBlock); err != nil {
			cmn.Exit(cmn.Fmt("Failed to add genesisBlock to Chain: %v", err))
		}
	}

	var accounts *account.Manager = nil
	var assets *asset.Registry = nil
	var pinStore *pin.Store = nil

	if config.Wallet.Enable {
		accounts_db := dbm.NewDB("account", config.DBBackend, config.DBDir())
		acc_utxos_db := dbm.NewDB("accountutxos", config.DBBackend, config.DBDir())
		pinStore = pin.NewStore(acc_utxos_db)
		err = pinStore.LoadAll(ctx)
		if err != nil {
			bytomlog.Error(ctx, err)
			return nil
		}

		pinHeight := store.Height()
		if pinHeight > 0 {
			pinHeight = pinHeight - 1
		}

		pins := []string{account.PinName, account.DeleteSpentsPinName}
		for _, p := range pins {
			err = pinStore.CreatePin(ctx, p, pinHeight)
			if err != nil {
				bytomlog.Fatalkv(ctx, bytomlog.KeyError, err)
			}
		}

		accounts = account.NewManager(accounts_db, chain, pinStore)
		go accounts.ProcessBlocks(ctx)

		assets_db := dbm.NewDB("asset", config.DBBackend, config.DBDir())
		assets = asset.NewRegistry(assets_db, chain)
	}
	//Todo HSM
	/*
		if config.HsmUrl != ""{
			// todo remoteHSM
			cmn.Exit(cmn.Fmt("not implement"))

		} else {
			hsm, err = pseudohsm.New(config.KeysDir())
			if err != nil {
				cmn.Exit(cmn.Fmt("initialize HSM failed: %v", err))
			}
		}*/

	hsm, err := pseudohsm.New(config.KeysDir())
	if err != nil {
		cmn.Exit(cmn.Fmt("initialize HSM failed: %v", err))
	}
	bcReactor := bc.NewBlockchainReactor(
		store,
		chain,
		txPool,
		accounts,
		assets,
		hsm,
		fastSync,
		pinStore)

	bcReactor.SetLogger(logger.With("module", "blockchain"))
	sw.AddReactor("BLOCKCHAIN", bcReactor)

	rpcInit(bcReactor, config)
	// Optionally, start the pex reactor
	var addrBook *p2p.AddrBook
	if config.P2P.PexReactor {
		addrBook = p2p.NewAddrBook(config.P2P.AddrBookFile(), config.P2P.AddrBookStrict)
		addrBook.SetLogger(p2pLogger.With("book", config.P2P.AddrBookFile()))
		pexReactor := p2p.NewPEXReactor(addrBook)
		pexReactor.SetLogger(p2pLogger)
		sw.AddReactor("PEX", pexReactor)
	}

	// add the event switch to all services
	// they should all satisfy events.Eventable
	//SetEventSwitch(eventSwitch, bcReactor, mempoolReactor, consensusReactor)

	// run the profile server
	profileHost := config.ProfListenAddress
	if profileHost != "" {

		go func() {
			logger.Error("Profile server", "error", http.ListenAndServe(profileHost, nil))
		}()
	}

	node := &Node{
		config: config,

		privKey:  privKey,
		sw:       sw,
		addrBook: addrBook,

		evsw:       eventSwitch,
		bcReactor:  bcReactor,
		blockStore: store,
		accounts:   accounts,
		assets:     assets,
	}
	node.BaseService = *cmn.NewBaseService(logger, "Node", node)
	return node
}

func (n *Node) OnStart() error {
	// Create & add listener
	protocol, address := ProtocolAndAddress(n.config.P2P.ListenAddress)
	l := p2p.NewDefaultListener(protocol, address, n.config.P2P.SkipUPNP, n.Logger.With("module", "p2p"))
	n.sw.AddListener(l)

	// Start the switch
	n.sw.SetNodeInfo(n.makeNodeInfo())
	n.sw.SetNodePrivKey(n.privKey)
	_, err := n.sw.Start()
	if err != nil {
		return err
	}

	// If seeds exist, add them to the address book and dial out
	if n.config.P2P.Seeds != "" {
		// dial out
		seeds := strings.Split(n.config.P2P.Seeds, ",")
		if err := n.DialSeeds(seeds); err != nil {
			return err
		}
	}
	// Run the RPC server
	if n.config.RPC.ListenAddress != "" {
		listeners, err := n.startRPC()
		if err != nil {
			return err
		}
		n.rpcListeners = listeners
	}

	return nil
}

func (n *Node) OnStop() {
	n.BaseService.OnStop()

	n.Logger.Info("Stopping Node")
	// TODO: gracefully disconnect from peers.
	n.sw.Stop()

	for _, l := range n.rpcListeners {
		n.Logger.Info("Closing rpc listener", "listener", l)
		if err := l.Close(); err != nil {
			n.Logger.Error("Error closing listener", "listener", l, "error", err)
		}
	}
}

func (n *Node) RunForever() {
	// Sleep forever and then...
	cmn.TrapSignal(func() {
		n.Stop()
	})
}

// Add the event switch to reactors, mempool, etc.
func SetEventSwitch(evsw types.EventSwitch, eventables ...types.Eventable) {
	for _, e := range eventables {
		e.SetEventSwitch(evsw)
	}
}

// Add a Listener to accept inbound peer connections.
// Add listeners before starting the Node.
// The first listener is the primary listener (in NodeInfo)
func (n *Node) AddListener(l p2p.Listener) {
	n.sw.AddListener(l)
}

// ConfigureRPC sets all variables in rpccore so they will serve
// rpc calls from this node
func (n *Node) ConfigureRPC() {
	rpccore.SetEventSwitch(n.evsw)
	rpccore.SetBlockStore(n.blockStore)
	rpccore.SetSwitch(n.sw)
	rpccore.SetAddrBook(n.addrBook)
	rpccore.SetLogger(n.Logger.With("module", "rpc"))
}

func (n *Node) startRPC() ([]net.Listener, error) {
	n.ConfigureRPC()
	listenAddrs := strings.Split(n.config.RPC.ListenAddress, ",")

	if n.config.RPC.Unsafe {
		rpccore.AddUnsafeRoutes()
	}

	// we may expose the rpc over both a unix and tcp socket
	listeners := make([]net.Listener, len(listenAddrs))
	for i, listenAddr := range listenAddrs {
		mux := http.NewServeMux()
		wm := rpcserver.NewWebsocketManager(rpccore.Routes, n.evsw)
		rpcLogger := n.Logger.With("module", "rpc-server")
		wm.SetLogger(rpcLogger)
		mux.HandleFunc("/websocket", wm.WebsocketHandler)
		rpcserver.RegisterRPCFuncs(mux, rpccore.Routes, rpcLogger)
		listener, err := rpcserver.StartHTTPServer(listenAddr, mux, rpcLogger)
		if err != nil {
			return nil, err
		}
		listeners[i] = listener
	}

	// we expose a simplified api over grpc for convenience to app devs
	grpcListenAddr := n.config.RPC.GRPCListenAddress
	if grpcListenAddr != "" {
		listener, err := grpccore.StartGRPCServer(grpcListenAddr)
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

func (n *Node) Switch() *p2p.Switch {
	return n.sw
}

func (n *Node) EventSwitch() types.EventSwitch {
	return n.evsw
}

func (n *Node) makeNodeInfo() *p2p.NodeInfo {
	nodeInfo := &p2p.NodeInfo{
		PubKey:  n.privKey.PubKey().Unwrap().(crypto.PubKeyEd25519),
		Moniker: n.config.Moniker,
		Network: "chain0",
		Version: version.Version,
		Other: []string{
			cmn.Fmt("wire_version=%v", wire.Version),
			cmn.Fmt("p2p_version=%v", p2p.Version),
		},
	}

	if !n.sw.IsListening() {
		return nodeInfo
	}

	p2pListener := n.sw.Listeners()[0]
	p2pHost := p2pListener.ExternalAddress().IP.String()
	p2pPort := p2pListener.ExternalAddress().Port
	rpcListenAddr := n.config.RPC.ListenAddress

	// We assume that the rpcListener has the same ExternalAddress.
	// This is probably true because both P2P and RPC listeners use UPnP,
	// except of course if the rpc is only bound to localhost
	nodeInfo.ListenAddr = cmn.Fmt("%v:%v", p2pHost, p2pPort)
	nodeInfo.Other = append(nodeInfo.Other, cmn.Fmt("rpc_addr=%v", rpcListenAddr))
	return nodeInfo
}

//------------------------------------------------------------------------------

func (n *Node) NodeInfo() *p2p.NodeInfo {
	return n.sw.NodeInfo()
}

func (n *Node) DialSeeds(seeds []string) error {
	return n.sw.DialSeeds(n.addrBook, seeds)
}

// Defaults to tcp
func ProtocolAndAddress(listenAddr string) (string, string) {
	protocol, address := "tcp", listenAddr
	parts := strings.SplitN(address, "://", 2)
	if len(parts) == 2 {
		protocol, address = parts[0], parts[1]
	}
	return protocol, address
}

//------------------------------------------------------------------------------
