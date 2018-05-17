package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/op/go-logging"
	"github.com/segmentio/ksuid"
	"github.com/tyler-smith/go-bip39"
	"gopkg.in/natefinch/lumberjack.v2"

	cmodels "github.com/textileio/textile-go/central/models"
	"github.com/textileio/textile-go/core/central"
	"github.com/textileio/textile-go/net"
	trepo "github.com/textileio/textile-go/repo"
	tconfig "github.com/textileio/textile-go/repo/config"
	"github.com/textileio/textile-go/repo/db"
	"github.com/textileio/textile-go/repo/photos"
	"github.com/textileio/textile-go/util"

	utilmain "gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/cmd/ipfs/util"
	oldcmds "gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/commands"
	"gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/core"
	"gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/core/coreapi"
	"gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/core/coreapi/interface"
	"gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/repo/config"
	"gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/repo/fsrepo"
	lockfile "gx/ipfs/QmatUACvrFK3xYg1nd2iLAKfz7Yy5YB56tnzBYHpqiUuhn/go-ipfs/repo/fsrepo/lock"

	"gx/ipfs/QmSFihvoND3eDaAYRCeLgLPt62yCPgMZs1NSZmKFEtJQQw/go-libp2p-floodsub"
	pstore "gx/ipfs/QmXauCuJzmzapetmC6W4TuDJLL1yFFrVzSHoWv8YdbmnxH/go-libp2p-peerstore"
	"gx/ipfs/QmZoWKhxUmZ2seW4BzX6fJkNR8hh9PsGModr7q171yq2SS/go-libp2p-peer"
	libp2p "gx/ipfs/QmaPbCnUMBohSGo3KnxEa2bHqyJVVeEEcwtqJAYxerieBo/go-libp2p-crypto"
)

const (
	Version = "0.0.1"
)

var fileLogFormat = logging.MustStringFormatter(
	`%{time:15:04:05.000} [%{shortfunc}] [%{level}] %{message}`,
)
var log = logging.MustGetLogger("core")

const roomRepublishInterval = time.Minute * 1
const pingRelayInterval = time.Second * 30
const pingTimeout = time.Second * 10
const pinTimeout = time.Minute * 1
const catTimeout = time.Second * 30

// Node is the single TextileNode instance
var Node *TextileNode

// ErrNodeRunning is an error for when node start is called on a running node
var ErrNodeRunning = errors.New("node is already running")

// ErrNodeNotRunning is an error for  when node stop is called on a nil node
var ErrNodeNotRunning = errors.New("node is not running")

// TextileNode is the main node interface for textile functionality
type TextileNode struct {
	// Context for issuing IPFS commands
	Context oldcmds.Context

	// IPFS node object
	IpfsNode *core.IpfsNode

	// The path to the openbazaar repo in the file system
	RepoPath string

	// Database for storing node specific data
	Datastore trepo.Datastore

	// Function to call for shutdown
	Cancel context.CancelFunc

	// The local decrypting gateway server
	GatewayProxy *http.Server

	// Map of hash passwords for the decrypting gateway server
	HashPasses map[string]string

	// The local password used to authenticate http gateway requests (username is TextileNode)
	GatewayPassword string

	// Signals for when we've left rooms
	LeftRoomChs map[string]chan struct{}

	// Signals for leaving rooms
	leaveRoomChs map[string]chan struct{}

	// IPFS configuration used to instantiate new ipfs nodes
	ipfsConfig *core.BuildCfg

	// Whether or not we're running on a mobile device
	isMobile bool

	// API URL of the central backup / recovery / pinning user service
	centralUserAPI string

	// Whether or not we've just inited, and never run, a "fresh" node :)
	fresh bool

	// Mutex for controlling lifecycle
	mux sync.Mutex

	// Captures stdout from ipfs packages
	stdOutLogger *util.StdOutLogger
}

// PhotoList contains a list of photo hashes
type PhotoList struct {
	Hashes []string `json:"hashes"`
}

// HashRequest represents a single-use gateway token
type HashRequest struct {
	Token    string `json:"token"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
}

// ThreadUpdate is used to notify listeners about updates in a thread
type ThreadUpdate struct {
	Cid      string `json:"cid"`
	Thread   string `json:"thread"`
	ThreadID string `json:"thread_id"`
}

// NodeConfig is used to configure the node
type NodeConfig struct {
	RepoPath      string
	CentralApiURL string
	IsMobile      bool
	IsServer      bool
	LogLevel      logging.Level
	LogFiles      bool
	SwarmPort     string
}

// NewNode creates a new TextileNode
func NewNode(config NodeConfig) (*TextileNode, error) {
	// TODO: shouldn't need to manually remove these
	repoLockFile := filepath.Join(config.RepoPath, lockfile.LockFile)
	os.Remove(repoLockFile)
	dsLockFile := filepath.Join(config.RepoPath, "datastore", "LOCK")
	os.Remove(dsLockFile)

	// log handling
	var backendFile *logging.LogBackend
	if config.LogFiles {
		w := &lumberjack.Logger{
			Filename:   path.Join(config.RepoPath, "logs", "textile.log"),
			MaxSize:    10, // megabytes
			MaxBackups: 3,
			MaxAge:     30, // days
		}
		backendFile = logging.NewLogBackend(w, "", 0)
	} else {
		backendFile = logging.NewLogBackend(os.Stdout, "", 0)
	}
	backendFileFormatter := logging.NewBackendFormatter(backendFile, fileLogFormat)
	logging.SetBackend(backendFileFormatter)
	logging.SetLevel(config.LogLevel, "")

	// capture stdout from ipfs packages
	stdOutLogger, err := util.NewStdOutLogger(logging.MustGetLogger("ipfs"))
	if err != nil {
		log.Errorf("error creating stdout logger: %s", err)
		return nil, err
	}

	// get database handle
	sqliteDB, err := db.Create(config.RepoPath, "")
	if err != nil {
		return nil, err
	}

	// we may be running in an uninitialized state.
	err = trepo.DoInit(config.RepoPath, config.IsMobile, sqliteDB.Config().Init, sqliteDB.Config().Configure)
	if err != nil && err != trepo.ErrRepoExists {
		return nil, err
	}

	// acquire the repo lock _before_ constructing a node. we need to make
	// sure we are permitted to access the resources (datastore, etc.)
	repo, err := fsrepo.Open(config.RepoPath)
	if err != nil {
		log.Errorf("error opening repo: %s", err)
		return nil, err
	}

	// determine the best routing
	var routingOption core.RoutingOption
	if config.IsMobile {
		// TODO: Determine best value for this setting on mobile
		// cfg.Swarm.DisableNatPortMap = true
		// NOTE: trying normal routing for a bit
		routingOption = core.DHTOption
	} else {
		routingOption = core.DHTOption
	}

	// assemble node config
	ncfg := &core.BuildCfg{
		Repo:      repo,
		Permanent: true, // temporary way to signify that node is permanent
		Online:    true,
		ExtraOpts: map[string]bool{
			"pubsub": true,
			"ipnsps": true,
			"mplex":  true,
		},
		Routing: routingOption,
	}

	// setup gateway
	gwAddr, err := repo.GetConfigKey("Addresses.Gateway")
	if err != nil {
		log.Errorf("error getting ipfs config: %s", err)
		return nil, err
	}
	gatewayProxy := &http.Server{
		Addr: gwAddr.(string),
	}

	// if a specific swarm port was selected, set it in the config
	if config.SwarmPort != "" {
		log.Infof("using specified swarm port: %s", config.SwarmPort)
		if err := tconfig.Update(repo, "Addresses.Swarm", []string{
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%s", config.SwarmPort),
			fmt.Sprintf("/ip6/::/tcp/%s", config.SwarmPort),
		}); err != nil {
			return nil, err
		}
	}

	// if this is a server node, apply the ipfs server profile
	if config.IsServer {
		if err := tconfig.Update(repo, "Addresses.NoAnnounce", tconfig.DefaultServerFilters); err != nil {
			return nil, err
		}
		if err := tconfig.Update(repo, "Swarm.AddrFilters", tconfig.DefaultServerFilters); err != nil {
			return nil, err
		}
		if err := tconfig.Update(repo, "Swarm.EnableRelayHop", true); err != nil {
			return nil, err
		}
		if err := tconfig.Update(repo, "Discovery.MDNS.Enabled", false); err != nil {
			return nil, err
		}
		log.Info("applied server profile")
	}

	// clean central api url
	if len(config.CentralApiURL) > 0 {
		ca := config.CentralApiURL
		if ca[len(ca)-1:] == "/" {
			ca = ca[0 : len(ca)-1]
		}
		config.CentralApiURL = ca
	}

	// finally, construct our node
	node := &TextileNode{
		RepoPath:        config.RepoPath,
		Datastore:       sqliteDB,
		GatewayProxy:    gatewayProxy,
		GatewayPassword: ksuid.New().String(),
		HashPasses:      make(map[string]string),
		LeftRoomChs:     make(map[string]chan struct{}),
		leaveRoomChs:    make(map[string]chan struct{}),
		ipfsConfig:      ncfg,
		isMobile:        config.IsMobile,
		centralUserAPI:  fmt.Sprintf("%s/api/v1/users", config.CentralApiURL),
		fresh:           true,
		stdOutLogger:    stdOutLogger,
	}

	// create default album
	da := node.Datastore.Albums().GetAlbumByName("default")
	if da == nil {
		err = node.CreateAlbum("", "default")
		if err != nil {
			log.Errorf("error creating default album: %s", err)
			return nil, err
		}
	}

	return node, nil
}

// Start the node
func (t *TextileNode) Start() error {
	t.mux.Lock()
	defer t.mux.Unlock()
	t.fresh = false
	if t.Online() {
		return ErrNodeRunning
	}
	log.Info("starting node...")

	// start capturing
	//t.stdOutLogger.Start()

	// raise file descriptor limit
	if err := utilmain.ManageFdLimit(); err != nil {
		log.Errorf("setting file descriptor limit: %s", err)
	}

	// check db
	if err := t.touchDB(); err != nil {
		return err
	}

	// check repo
	if t.ipfsConfig.Repo == nil {
		log.Debug("re-opening repo...")
		repo, err := fsrepo.Open(t.RepoPath)
		if err != nil {
			log.Errorf("error re-opening repo: %s", err)
			return err
		}
		t.ipfsConfig.Repo = repo
	}

	// start the ipfs node
	log.Debug("creating an ipfs node...")
	cctx, cancel := context.WithCancel(context.Background())
	t.Cancel = cancel
	nd, err := core.NewNode(cctx, t.ipfsConfig)
	if err != nil {
		log.Errorf("error creating ipfs node: %s", err)
		return err
	}
	nd.SetLocal(false)

	// print swarm addresses
	if err = printSwarmAddrs(nd); err != nil {
		log.Errorf("failed to read listening addresses: %s", err)
	}

	// build the node
	ctx := oldcmds.Context{}
	ctx.Online = true
	ctx.ConfigRoot = t.RepoPath
	ctx.LoadConfig = func(path string) (*config.Config, error) {
		return fsrepo.ConfigAt(t.RepoPath)
	}
	ctx.ConstructNode = func() (*core.IpfsNode, error) {
		return nd, nil
	}
	t.Context = ctx
	t.IpfsNode = nd

	// construct decrypting http gateway
	var gwpErrc <-chan error
	gwpErrc, err = t.startGateway()
	if err != nil {
		log.Errorf("error starting decrypting gateway: %s", err)
		return err
	}
	go func() {
		for {
			select {
			case err, ok := <-gwpErrc:
				if err != nil && err.Error() != "http: Server closed" {
					log.Errorf("gateway error: %s", err)
				}
				if !ok {
					log.Info("decrypting gateway was shutdown")
					return
				}
			}
		}
	}()

	// every min, send out latest room updates
	go t.startRepublishing()

	// every 30s, ping the relay
	go t.startPingingRelay()

	// dunzo
	if t.isMobile {
		log.Info("mobile node is ready")
	} else {
		log.Info("desktop node is ready")
	}
	return nil
}

// Stop the node
func (t *TextileNode) Stop() error {
	t.mux.Lock()
	defer t.mux.Unlock()
	if !t.Online() {
		return ErrNodeNotRunning
	}
	log.Info("stopping node...")

	// shutdown the gateway
	cgCtx, cancelCGW := context.WithCancel(context.Background())
	if err := t.GatewayProxy.Shutdown(cgCtx); err != nil {
		log.Errorf("error shutting down gateway: %s", err)
		return err
	}

	// close ipfs node command context
	t.Context.Close()

	// cancel textile node background context
	t.Cancel()

	// close the ipfs node
	if err := t.IpfsNode.Close(); err != nil {
		log.Errorf("error closing ipfs node: %s", err)
		return err
	}

	// force the gateway closed if it's not already closed
	cancelCGW()

	// close db connection
	t.Datastore.Close()
	dsLockFile := filepath.Join(t.RepoPath, "datastore", "LOCK")
	if err := os.Remove(dsLockFile); err != nil {
		log.Errorf("error removing ds lock: %s", err)
	}

	// clean up node
	t.IpfsNode = nil
	t.ipfsConfig.Repo = nil

	// stop capturing
	//t.stdOutLogger.Stop()

	return nil
}

func (t *TextileNode) Online() bool {
	return t.fresh || t.IpfsNode != nil
}

// StartGarbageCollection starts auto garbage cleanup
// TODO: verify this is finding the correct repo, might be using IPFS_PATH
func (t *TextileNode) StartGarbageCollection() (<-chan error, error) {
	if !t.Online() {
		return nil, ErrNodeNotRunning
	}
	if t.isMobile {
		return nil, errors.New("services not available on mobile")
	}

	// repo blockstore GC
	var gcErrc <-chan error
	var err error
	gcErrc, err = runGC(t.IpfsNode.Context(), t.IpfsNode)
	if err != nil {
		log.Errorf("error starting gc: %s", err)
		return nil, err
	}

	return gcErrc, nil
}

// SignUp requests a new username and token from the central api and saves them locally
func (t *TextileNode) SignUp(reg *cmodels.Registration) error {
	// check db
	if err := t.touchDB(); err != nil {
		return err
	}
	log.Debugf("signup: %s %s %s %s %s", reg.Username, "xxxxxx", reg.Identity.Type, reg.Identity.Value, reg.Referral)

	// remote signup
	res, err := central.SignUp(reg, t.centralUserAPI)
	if err != nil {
		log.Errorf("signup error: %s", err)
		return err
	}
	if res.Error != nil {
		log.Errorf("signup error from central: %s", *res.Error)
		return errors.New(*res.Error)
	}

	// local signin
	if err := t.Datastore.Config().SignIn(reg.Username, res.Session.AccessToken, res.Session.RefreshToken); err != nil {
		log.Errorf("local signin error: %s", err)
		return err
	}
	return nil
}

// SignIn requests a token with a username from the central api and saves them locally
func (t *TextileNode) SignIn(creds *cmodels.Credentials) error {
	// check db
	if err := t.touchDB(); err != nil {
		return err
	}
	log.Debugf("signin: %s %s", creds.Username, "xxxxxx")

	// remote signin
	res, err := central.SignIn(creds, t.centralUserAPI)
	if err != nil {
		log.Errorf("signin error: %s", err)
		return err
	}
	if res.Error != nil {
		log.Errorf("signin error from central: %s", *res.Error)
		return errors.New(*res.Error)
	}

	// local signin
	if err := t.Datastore.Config().SignIn(creds.Username, res.Session.AccessToken, res.Session.RefreshToken); err != nil {
		log.Errorf("local signin error: %s", err)
		return err
	}
	return nil
}

// SignOut deletes the locally saved user info (username and tokens)
func (t *TextileNode) SignOut() error {
	// check db
	if err := t.touchDB(); err != nil {
		return err
	}
	log.Debug("signing out...")

	// remote is stateless, so we just ditch the local token
	if err := t.Datastore.Config().SignOut(); err != nil {
		log.Errorf("local signout error: %s", err)
		return err
	}
	return nil
}

// IsSignedIn returns whether or not a user is signed in
func (t *TextileNode) IsSignedIn() (bool, error) {
	// check db
	if err := t.touchDB(); err != nil {
		return false, err
	}
	_, err := t.Datastore.Config().GetUsername()
	return err == nil, nil
}

func (t *TextileNode) JoinThread(mnemonic string, name string) error {
	if err := t.touchDB(); err != nil {
		return err
	}
	if mnemonic == "" {
		// TODO: Return error
		return errors.New("mnemonic must not be empty")
	}

	ba := t.Datastore.Albums().GetAlbumByName(name)
	if ba == nil {
		err := t.CreateAlbum(mnemonic, name)
		if err != nil {
			log.Errorf("error creating album %s: %s", name, err)
			return err
		}
	} else {
		log.Debugf("updating album: %s", name)
		return t.RegisterAlbum(mnemonic, name)
	}
	return nil
}

// GetUsername returns the current user's username
func (t *TextileNode) GetUsername() (string, error) {
	// check db
	if err := t.touchDB(); err != nil {
		return "", err
	}
	un, err := t.Datastore.Config().GetUsername()
	if err != nil {
		return "", err
	}
	return un, nil
}

// GetAccessToken returns the current access_token (jwt) for central
func (t *TextileNode) GetAccessToken() (string, error) {
	// check db
	if err := t.touchDB(); err != nil {
		return "", err
	}
	at, _, err := t.Datastore.Config().GetTokens()
	if err != nil {
		return "", err
	}
	return at, nil
}

// JoinRoom with a given id
func (t *TextileNode) JoinRoom(id string, datac chan ThreadUpdate) {
	if !t.Online() {
		return
	}

	// create the subscription
	sub, err := t.IpfsNode.Floodsub.Subscribe(id)
	if err != nil {
		log.Errorf("error creating subscription: %s", err)
		return
	}
	log.Infof("joined room: %s\n", id)

	t.leaveRoomChs[id] = make(chan struct{})
	t.LeftRoomChs[id] = make(chan struct{})

	api := coreapi.NewCoreAPI(t.IpfsNode)
	ctx, cancel := context.WithCancel(context.Background())
	leave := func() {
		cancel()
		close(t.LeftRoomChs[id])
		delete(t.LeftRoomChs, id)
		delete(t.leaveRoomChs, id)
		log.Infof("left room: %s\n", sub.Topic())
	}

	defer func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("room data channel already closed")
			}
		}()
		close(datac)
	}()
	go func() {
		for {
			// unload new message
			msg, err := sub.Next(ctx)
			if err == io.EOF || err == context.Canceled {
				log.Debugf("room subscription ended: %s", err)
				return
			} else if err != nil {
				log.Debugf(err.Error())
				return
			}

			// handle the update
			go func(msg *floodsub.Message) {
				if err = t.handleRoomUpdate(msg, id, api, datac); err != nil {
					log.Errorf("error handling room update: %s", err)
				}
			}(msg)
		}
	}()

	// block so we can shutdown with the leave room signal
	for {
		select {
		case <-t.leaveRoomChs[id]:
			leave()
			return
		case <-t.IpfsNode.Context().Done():
			leave()
			return
		}
	}
}

// LeaveRoom with a given id
func (t *TextileNode) LeaveRoom(id string) {
	if !t.Online() {
		return
	}

	if t.leaveRoomChs[id] == nil {
		return
	}
	close(t.leaveRoomChs[id])
}

// WaitForRoom to join
func (t *TextileNode) WaitForRoom() {
	if !t.Online() {
		return
	}

	// we're in a lonesome state here, we can just sub to our own
	// peer id and hope somebody sends us a priv key to join a room with
	rid := t.IpfsNode.Identity.Pretty()
	sub, err := t.IpfsNode.Floodsub.Subscribe(rid)
	if err != nil {
		log.Errorf("error creating subscription: %s", err)
		return
	}
	log.Infof("waiting for room at own peer id: %s\n", rid)

	ctx, cancel := context.WithCancel(context.Background())
	cancelCh := make(chan struct{})
	go func() {
		for {
			msg, err := sub.Next(ctx)
			if err == io.EOF || err == context.Canceled {
				log.Debugf("wait subscription ended: %s", err)
				return
			} else if err != nil {
				log.Debugf(err.Error())
				return
			}
			from := msg.GetFrom().Pretty()
			log.Infof("got pairing request from: %s\n", from)

			// get private peer key and decrypt the phrase
			sk, err := t.UnmarshalPrivatePeerKey()
			if err != nil {
				log.Errorf("error unmarshaling priv peer key: %s", err)
				return
			}
			p, err := net.Decrypt(sk, msg.GetData())
			if err != nil {
				log.Errorf("error decrypting msg data: %s", err)
				return
			}
			ps := string(p)
			log.Debugf("decrypted mnemonic phrase as: %s\n", ps)

			// create a new album for the room
			// TODO: let user name this or take phone's name, e.g., bob's iphone
			// TODO: or auto name it, cause this means only one pairing can happen
			t.CreateAlbum(ps, "mobile")

			// we're done
			close(cancelCh)
		}
	}()

	for {
		select {
		case <-cancelCh:
			cancel()
			return
		case <-t.IpfsNode.Context().Done():
			cancel()
			return
		}
	}
}

// CreateAlbum creates an album with a given name and mnemonic words
func (t *TextileNode) CreateAlbum(mnemonic string, name string) error {
	// check db
	if err := t.touchDB(); err != nil {
		return err
	}
	log.Debugf("creating a new album: %s", name)

	// use phrase if provided
	if mnemonic == "" {
		var err error
		mnemonic, err = createMnemonic(bip39.NewEntropy, bip39.NewMnemonic)
		if err != nil {
			log.Errorf("error creating mnemonic: %s", err)
			return err
		}
		log.Debugf("generating %v-bit Ed25519 keypair for: %s", trepo.NBitsForKeypair, name)
	} else {
		log.Debugf("regenerating Ed25519 keypair from mnemonic phrase for: %s", name)
	}
	return t.RegisterAlbum(mnemonic, name)
}

func (t *TextileNode) RegisterAlbum(mnemonic string, name string) error {
	// create the bip39 seed from the phrase
	seed := bip39.NewSeed(mnemonic, "")
	kb, err := identityKeyFromSeed(seed, trepo.NBitsForKeypair)
	if err != nil {
		log.Errorf("error creating identity from seed: %s", err)
		return err
	}

	// convert to a libp2p crypto private key
	sk, err := libp2p.UnmarshalPrivateKey(kb)
	if err != nil {
		log.Errorf("error unmarshaling private key: %s", err)
		return err
	}

	// we need the resultant peer id to use as the album's id
	id, err := peer.IDFromPrivateKey(sk)
	if err != nil {
		log.Errorf("error getting id from priv key: %s", err)
		return err
	}

	// finally, create the album
	album := &trepo.PhotoAlbum{
		Id:       id.Pretty(),
		Key:      sk,
		Mnemonic: mnemonic,
		Name:     name,
	}
	return t.Datastore.Albums().Put(album)
}

// AddPhoto adds a photo and its thumbnail to an album
// TODO: Make this available offline
func (t *TextileNode) AddPhoto(path string, thumb string, album string, caption string) (*net.MultipartRequest, error) {
	if !t.Online() {
		return nil, ErrNodeNotRunning
	}
	log.Debugf("adding photo %s to %s", path, album)

	// read file from disk
	p, err := os.Open(path)
	if err != nil {
		log.Errorf("error opening photo: %s", err)
		return nil, err
	}
	defer p.Close()

	th, err := os.Open(thumb)
	if err != nil {
		log.Errorf("error opening thumb: %s", err)
		return nil, err
	}
	defer th.Close()

	// get album private key
	a := t.Datastore.Albums().GetAlbumByName(album)
	if a == nil {
		err = errors.New(fmt.Sprintf("could not find album: %s", album))
		log.Error(err.Error())
		return nil, err
	}

	// get last photo update which has local true
	var lc string
	recent := t.Datastore.Photos().GetPhotos("", 1, "album='"+a.Id+"' and local=1")
	if len(recent) > 0 {
		lc = recent[0].Cid
		log.Debugf("found last hash: %s", lc)
	}

	// get username
	un, err := t.Datastore.Config().GetUsername()
	if err != nil {
		log.Errorf("username not found (not signed in)")
		un = ""
	}

	// add it
	// TODO: clean up this nasty method signature
	mr, md, err := photos.Add(t.IpfsNode, a.Key.GetPublic(), p, th, lc, un, caption)
	if err != nil {
		log.Errorf("error adding photo: %s", err)
		return nil, err
	}

	// index
	log.Debugf("indexing %s...", mr.Boundary)
	set := &trepo.PhotoSet{
		Cid:      mr.Boundary,
		LastCid:  lc,
		AlbumID:  a.Id,
		MetaData: *md,
		Caption:  caption,
		IsLocal:  true,
	}
	err = t.Datastore.Photos().Put(set)
	if err != nil {
		log.Errorf("error indexing photo: %s", err)
		return nil, err
	}

	// publish
	go func() {
		err = t.IpfsNode.Floodsub.Publish(a.Id, []byte(mr.Boundary))
		if err != nil {
			log.Errorf("error publishing photo update: %s", err)
		}
		log.Debugf("published update to %s", a.Id)
	}()

	return mr, nil
}

// SharePhoto re-encrypts a photo from an existing album and shares it into a different album
// TODO: Make this available offline
func (t *TextileNode) SharePhoto(hash string, album string, caption string) (*net.MultipartRequest, error) {
	if !t.Online() {
		return nil, ErrNodeNotRunning
	}
	log.Debugf("sharing photo %s to %s...", hash, album)

	// get the photo
	set, a, err := t.LoadPhotoAndAlbum(hash)
	if err != nil {
		log.Error(err.Error())
		return nil, err
	}

	// get dest album
	na := t.Datastore.Albums().GetAlbumByName(album)
	if na == nil {
		return nil, errors.New(fmt.Sprintf("could not find album: %s", album))
	}

	// check if album is diff
	if a.Id == na.Id {
		return nil, errors.New(fmt.Sprintf("photo already in album: %s", album))
	}

	// get photo data
	pb, err := t.GetFile(fmt.Sprintf("%s/photo", hash), a)
	if err != nil {
		return nil, err
	}
	tb, err := t.GetFile(fmt.Sprintf("%s/thumb", hash), a)
	if err != nil {
		return nil, err
	}

	// temp write to disk
	ppath := filepath.Join(t.RepoPath, "tmp", set.MetaData.Name+set.MetaData.Ext)
	tpath := filepath.Join(t.RepoPath, "tmp", "thumb_"+set.MetaData.Name+set.MetaData.Ext)
	err = ioutil.WriteFile(ppath, pb, 0644)
	if err != nil {
		return nil, err
	}
	defer func() {
		err := os.Remove(ppath)
		if err != nil {
			log.Errorf("error cleaning up shared photo path: %s", ppath)
		}
	}()
	err = ioutil.WriteFile(tpath, tb, 0644)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = os.Remove(tpath)
		if err != nil {
			log.Errorf("error cleaning up shared thumb path: %s", tpath)
		}
	}()

	return t.AddPhoto(ppath, tpath, album, caption)
}

// GetHashRequest returns a single-use token for requesting content via the gateway
func (t *TextileNode) GetHashRequest(hash string) HashRequest {
	token := ksuid.New().String()
	t.HashPasses[hash] = token
	return HashRequest{
		Token:    token,
		Protocol: "http",
		Host:     t.GatewayProxy.Addr,
	}
}

// GetPhotos paginates photos from the datastore
func (t *TextileNode) GetPhotos(offsetId string, limit int, album string) *PhotoList {
	// check db
	if err := t.touchDB(); err != nil {
		return nil
	}
	log.Debugf("getting photos: offsetId: %s, limit: %d, album: %s", offsetId, limit, album)

	// query for available hashes
	a := t.Datastore.Albums().GetAlbumByName(album)
	if a == nil {
		return &PhotoList{Hashes: make([]string, 0)}
	}
	list := t.Datastore.Photos().GetPhotos(offsetId, limit, "album='"+a.Id+"'")

	// return json list of hashes
	res := &PhotoList{
		Hashes: make([]string, len(list)),
	}
	for i := range list {
		res.Hashes[i] = list[i].Cid
	}
	log.Debugf("found %d photos in thread %s", len(list), album)

	return res
}

// GetFile cats data from the ipfs node.
// e.g., Qm../thumb, Qm../photo, Qm../meta, Qm../caption
// Note: album is looked up if not present
// TODO: shouldn't this be available offline for local (pinned content)?
func (t *TextileNode) GetFile(path string, album *trepo.PhotoAlbum) ([]byte, error) {
	if !t.Online() {
		return nil, ErrNodeNotRunning
	}

	// get bytes
	cb, err := t.getDataAtPath(path)
	if err != nil {
		log.Errorf("error getting file data: %s", err)
		return nil, err
	}

	// normalize path
	ip, err := coreapi.ParsePath(path)
	if err != nil {
		log.Errorf("error parsing path: %s", err)
		return nil, err
	}

	// parse root hash of path
	tmp := strings.Split(ip.String(), "/")
	if len(tmp) < 3 {
		err := errors.New(fmt.Sprintf("bad path: %s", path))
		log.Error(err.Error())
		return nil, err
	}
	ci := tmp[2]

	// look up key for decryption
	if album == nil {
		_, album, err = t.LoadPhotoAndAlbum(ci)
		if err != nil {
			log.Error(err.Error())
			return nil, err
		}
	}

	// finally, decrypt
	b, err := net.Decrypt(album.Key, cb)
	if err != nil {
		log.Errorf("error decrypting file: %s", err)
		return nil, err
	}

	return b, err
}

// GetFileBase64 returns data encoded as base64 under an ipfs path
func (t *TextileNode) GetFileBase64(path string) (string, error) {
	b, err := t.GetFile(path, nil)
	if err != nil {
		return "error", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// GetMetaData returns metadata under a hash
// TODO: shouldn't this be available offline for local (pinned content)?
func (t *TextileNode) GetMetaData(hash string, album *trepo.PhotoAlbum) (*photos.Metadata, error) {
	if !t.Online() {
		return nil, ErrNodeNotRunning
	}
	b, err := t.GetFile(fmt.Sprintf("%s/meta", hash), album)
	if err != nil {
		log.Errorf("error getting meta file with hash: %s: %s", hash, err)
		return nil, err
	}
	var data *photos.Metadata
	err = json.Unmarshal(b, &data)
	if err != nil {
		log.Errorf("error unmarshaling meta file with hash: %s: %s", hash, err)
		return nil, err
	}

	return data, nil
}

// GetLastHash return the caption under a hash
// TODO: shouldn't this be available offline for local (pinned content)?
func (t *TextileNode) GetCaption(hash string, album *trepo.PhotoAlbum) (string, error) {
	if !t.Online() {
		return "", ErrNodeNotRunning
	}
	b, err := t.GetFile(fmt.Sprintf("%s/caption", hash), album)
	if err != nil {
		log.Errorf("error getting caption file with hash: %s: %s", hash, err)
		return "", err
	}

	return string(b), nil
}

// GetLastHash return the last update's root cid under a hash
// TODO: shouldn't this be available offline for local (pinned content)?
func (t *TextileNode) GetLastHash(hash string, album *trepo.PhotoAlbum) (string, error) {
	if !t.Online() {
		return "", ErrNodeNotRunning
	}
	b, err := t.GetFile(fmt.Sprintf("%s/last", hash), album)
	if err != nil {
		log.Errorf("error getting last hash file with hash: %s: %s", hash, err)
		return "", err
	}

	return string(b), nil
}

// LoadPhotoAndAlbum get the indexed photo and album for a hash
func (t *TextileNode) LoadPhotoAndAlbum(hash string) (*trepo.PhotoSet, *trepo.PhotoAlbum, error) {
	if !t.Online() {
		return nil, nil, ErrNodeNotRunning
	}
	ph := t.Datastore.Photos().GetPhoto(hash)
	if ph == nil {
		return nil, nil, errors.New(fmt.Sprintf("photo %s not found", hash))
	}
	album := t.Datastore.Albums().GetAlbum(ph.AlbumID)
	if album == nil {
		return nil, nil, errors.New(fmt.Sprintf("could not find album: %s", ph.AlbumID))
	}
	return ph, album, nil
}

// UnmarshalPrivatePeerKey returns a PrivKey instance from the base64 encoded ipfs peer key
func (t *TextileNode) UnmarshalPrivatePeerKey() (libp2p.PrivKey, error) {
	if !t.Online() {
		return nil, ErrNodeNotRunning
	}
	cfg, err := t.Context.GetConfig()
	if err != nil {
		return nil, err
	}
	skb, err := base64.StdEncoding.DecodeString(cfg.Identity.PrivKey)
	if err != nil {
		return nil, err
	}
	sk, err := libp2p.UnmarshalPrivateKey(skb)
	if err != nil {
		return nil, err
	}

	// check
	id2, err := peer.IDFromPrivateKey(sk)
	if err != nil {
		return nil, err
	}
	if id2 != t.IpfsNode.Identity {
		return nil, fmt.Errorf("private key in config does not match id: %s != %s", t.IpfsNode.Identity, id2)
	}

	return sk, nil
}

// GetPublicPeerKeyString returns the base64 encoded public ipfs peer key
func (t *TextileNode) GetPublicPeerKeyString() (string, error) {
	sk, err := t.UnmarshalPrivatePeerKey()
	if err != nil {
		log.Errorf("error unmarshaling priv peer key: %s", err)
		return "", err
	}
	pkb, err := sk.GetPublic().Bytes()
	if err != nil {
		log.Errorf("error getting pub key bytes: %s", err)
		return "", err
	}

	return base64.StdEncoding.EncodeToString(pkb), nil
}

// PingPeer pings a peer num times, returning the result to out chan
func (t *TextileNode) PingPeer(addrs string, num int, out chan string) error {
	if !t.Online() {
		return ErrNodeNotRunning
	}
	addr, pid, err := parsePeerParam(addrs)
	if addr != nil {
		t.IpfsNode.Peerstore.AddAddr(pid, addr, pstore.TempAddrTTL) // temporary
	}

	if len(t.IpfsNode.Peerstore.Addrs(pid)) == 0 {
		// Make sure we can find the node in question
		log.Debugf("looking up peer: %s", pid.Pretty())

		ctx, cancel := context.WithTimeout(t.IpfsNode.Context(), pingTimeout)
		defer cancel()
		p, err := t.IpfsNode.Routing.FindPeer(ctx, pid)
		if err != nil {
			err = fmt.Errorf("peer lookup error: %s", err)
			log.Errorf(err.Error())
			return err
		}
		t.IpfsNode.Peerstore.AddAddrs(p.ID, p.Addrs, pstore.TempAddrTTL)
	}

	ctx, cancel := context.WithTimeout(t.IpfsNode.Context(), pingTimeout*time.Duration(num))
	defer cancel()
	pings, err := t.IpfsNode.Ping.Ping(ctx, pid)
	if err != nil {
		log.Errorf("error pinging peer %s: %s", pid.Pretty(), err)
		return err
	}

	var done bool
	var total time.Duration
	for i := 0; i < num && !done; i++ {
		select {
		case <-ctx.Done():
			done = true
			close(out)
			break
		case t, ok := <-pings:
			if !ok {
				done = true
				close(out)
				break
			}
			total += t
			msg := fmt.Sprintf("ping %s completed after %f seconds", pid.Pretty(), t.Seconds())
			select {
			case out <- msg:
			default:
			}
			log.Debug(msg)
			time.Sleep(time.Second)
		}
	}

	return nil
}

// registerGatewayHandler registers a handler for the gateway
func (t *TextileNode) registerGatewayHandler() {
	defer func() {
		if recover() != nil {
			log.Error("gateway handler already registered")
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		log.Debugf("gateway request: %s", r.URL.RequestURI())
		if ok == false {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			w.WriteHeader(401)
			return
		}

		// parse root hash of path
		tmp := strings.Split(r.URL.Path, "/")
		if len(tmp) < 3 {
			err := errors.New(fmt.Sprintf("bad path: %s", r.URL.Path))
			log.Error(err.Error())
			return
		}

		ci := strings.Join(tmp[2:], "/")
		if password != t.HashPasses[ci] {
			log.Debugf("wrong password: %s", ci, t.HashPasses[ci])
			w.WriteHeader(401)
			return
		}

		// invalidate previous password
		// t.HashPasses[username] = ksuid.New().String()

		b, err := t.GetFile(r.URL.Path, nil)
		if err != nil {
			log.Errorf("error decrypting path %s: %s", r.URL.Path, err)
			w.WriteHeader(400)
			return
		}

		w.Write(b)
	})
}

// startGateway starts the secure HTTP gatway server
func (t *TextileNode) startGateway() (<-chan error, error) {
	// try to register our handler
	t.registerGatewayHandler()

	// Start the HTTPS server in a goroutine
	errc := make(chan error)
	go func() {
		errc <- t.GatewayProxy.ListenAndServe()
		close(errc)
	}()
	log.Infof("decrypting gateway (readonly) server listening at %s\n", t.GatewayProxy.Addr)

	return errc, nil
}

// startRepublishing continuously publishes the latest update in each thread
func (t *TextileNode) startRepublishing() {
	// do it once right away
	t.republishLatestUpdates()

	// create a never-ending ticker
	ticker := time.NewTicker(roomRepublishInterval)
	defer func() {
		ticker.Stop()
		defer func() {
			if recover() != nil {
				log.Error("republishing ticker already stopped")
			}
		}()
	}()
	go func() {
		for range ticker.C {
			t.republishLatestUpdates()
		}
	}()

	// we can stop when the node stops
	for {
		if !t.Online() {
			return
		}
		select {
		case <-t.IpfsNode.Context().Done():
			log.Info("republishing stopped")
			return
		}
	}
}

// republishLatestUpdates interates through albums, publishing each ones latest update
func (t *TextileNode) republishLatestUpdates() {
	// do this for each album
	albums := t.Datastore.Albums().GetAlbums("")
	for _, a := range albums {
		// find latest local update
		recent := t.Datastore.Photos().GetPhotos("", 1, "album='"+a.Id+"' and local=1")
		if len(recent) == 0 {
			return
		}
		latest := recent[0].Cid

		// publish it
		go func(id string, hash string) {
			log.Debugf("starting re-publish...")
			if err := t.IpfsNode.Floodsub.Publish(id, []byte(hash)); err != nil {
				log.Errorf("error re-publishing update: %s", err)
			}
			log.Debugf("re-published %s to %s", hash, id)
		}(a.Id, latest)
	}
}

// startPingingRelay continuously pings a shared relay
func (t *TextileNode) startPingingRelay() {
	// do it once right away
	err := t.PingPeer(tconfig.RemoteRelayNode, 1, make(chan string))
	if err != nil {
		log.Errorf("ping relay failed: %s", err)
	}

	// create a never-ending ticker
	ticker := time.NewTicker(pingRelayInterval)
	defer func() {
		ticker.Stop()
		defer func() {
			if recover() != nil {
				log.Error("ping relay ticker already stopped")
			}
		}()
	}()
	go func() {
		for range ticker.C {
			err := t.PingPeer(tconfig.RemoteRelayNode, 1, make(chan string))
			if err != nil {
				log.Errorf("ping relay failed: %s", err)
			}
		}
	}()

	// we can stop when the node stops
	for {
		if !t.Online() {
			return
		}
		select {
		case <-t.IpfsNode.Context().Done():
			log.Info("pinging relay stopped")
			return
		}
	}
}

// getDataAtPath cats any data under an ipfs path
func (t *TextileNode) getDataAtPath(path string) ([]byte, error) {
	// convert string to an ipfs path
	ip, err := coreapi.ParsePath(path)
	if err != nil {
		return nil, err
	}

	api := coreapi.NewCoreAPI(t.IpfsNode)
	ctx, cancel := context.WithTimeout(t.IpfsNode.Context(), catTimeout)
	defer cancel()
	r, err := api.Unixfs().Cat(ctx, ip)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return ioutil.ReadAll(r)
}

// handleRoomUpdate tries to recursively process an update sent to a thread
func (t *TextileNode) handleRoomUpdate(msg *floodsub.Message, aid string, api iface.CoreAPI, datac chan ThreadUpdate) error {
	// unpack from
	from := msg.GetFrom().Pretty()
	if from == t.IpfsNode.Identity.Pretty() {
		return nil
	}

	// unpack message data
	data := string(msg.GetData())

	// determine if this is from a relay node
	tmp := strings.Split(data, ":")
	var hash string
	if len(tmp) > 1 && tmp[0] == "relay" {
		hash = tmp[1]
		from = fmt.Sprintf("relay:%s", from)
	} else {
		hash = tmp[0]
	}
	log.Debugf("got update from %s in room %s", from, aid)

	// recurse back in time starting at this hash
	err := t.handleHash(hash, aid, api, datac)
	if err != nil {
		return err
	}

	return nil
}

// handleHash tries to process an update sent to a thread
func (t *TextileNode) handleHash(hash string, aid string, api iface.CoreAPI, datac chan ThreadUpdate) error {
	// look up the album
	a := t.Datastore.Albums().GetAlbum(aid)
	if a == nil {
		err := errors.New(fmt.Sprintf("could not find album with id: %s", aid))
		return err
	}

	// first update?
	if hash == "" {
		log.Debugf("found genesis update, aborting")
		return nil
	}
	log.Debugf("handling update: %s...", hash)

	// check if we aleady have this hash
	set := t.Datastore.Photos().GetPhoto(hash)
	if set != nil {
		log.Debugf("update %s exists, aborting", hash)
		return nil
	}

	// pin the dag structure
	log.Debugf("pinning %s...", hash)
	if err := t.pinPath(hash, api, false); err != nil {
		return err
	}

	// pin the thumbnail
	log.Debugf("pinning %s/thumb...", hash)
	if err := t.pinPath(fmt.Sprintf("%s/thumb", hash), api, false); err != nil {
		return err
	}

	// pin the meta
	log.Debugf("pinning %s/meta...", hash)
	if err := t.pinPath(fmt.Sprintf("%s/meta", hash), api, false); err != nil {
		return err
	}

	// pin the last hash
	log.Debugf("pinning %s/last...", hash)
	if err := t.pinPath(fmt.Sprintf("%s/last", hash), api, false); err != nil {
		return err
	}

	// pin the caption (may not exist, ignore error)
	log.Debugf("pinning %s/caption...", hash)
	t.pinPath(fmt.Sprintf("%s/caption", hash), api, false)

	// unpack data set
	log.Debugf("unpacking %s...", hash)
	md, err := t.GetMetaData(hash, a)
	if err != nil {
		return err
	}
	last, err := t.GetLastHash(hash, a)
	if err != nil {
		return err
	}
	caption, err := t.GetCaption(hash, a)
	if err != nil {
		caption = ""
	}

	// index
	log.Debugf("indexing %s...", hash)
	set = &trepo.PhotoSet{
		Cid:      hash,
		LastCid:  last,
		AlbumID:  aid,
		MetaData: *md,
		Caption:  caption,
		IsLocal:  false,
	}
	err = t.Datastore.Photos().Put(set)
	if err != nil {
		return err
	}

	// don't block on the send since nobody might be listening
	select {
	case datac <- ThreadUpdate{Cid: hash, Thread: a.Name, ThreadID: a.Id}:
	default:
	}

	// check last hash
	return t.handleHash(last, aid, api, datac)
}

// pinPath takes an ipfs path string and pins it
func (t *TextileNode) pinPath(path string, api iface.CoreAPI, recursive bool) error {
	ip, err := coreapi.ParsePath(path)
	if err != nil {
		log.Errorf("error pinning path: %s, recursive: %t: %s", path, recursive, err)
		return err
	}
	ctx, cancel := context.WithTimeout(t.IpfsNode.Context(), pinTimeout)
	defer cancel()
	return api.Pin().Add(ctx, ip, api.Pin().WithRecursive(recursive))
}

// touchDB ensures that we have a good db connection
func (t *TextileNode) touchDB() error {
	if err := t.Datastore.Ping(); err != nil {
		log.Debug("re-opening datastore...")
		sqliteDB, err := db.Create(t.RepoPath, "")
		if err != nil {
			log.Errorf("error re-opening datastore: %s", err)
			return err
		}
		t.Datastore = sqliteDB
	}
	return nil
}
