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
	"strconv"
	"strings"
	"time"

	"github.com/op/go-logging"
	"github.com/segmentio/ksuid"
	"github.com/tyler-smith/go-bip39"
	"gopkg.in/natefinch/lumberjack.v2"

	cmodels "github.com/textileio/textile-go/central/models"
	"github.com/textileio/textile-go/core/central"
	"github.com/textileio/textile-go/net"
	trepo "github.com/textileio/textile-go/repo"
	"github.com/textileio/textile-go/repo/db"
	"github.com/textileio/textile-go/repo/photos"
	"github.com/textileio/textile-go/ssl"

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
	"gx/ipfs/Qmej7nf81hi2x2tvjRBF3mcp74sQyuDH4VMYDGd1YtXjb2/go-block-format"
)

const (
	Version = "0.0.1"
)

var fileLogFormat = logging.MustStringFormatter(
	`%{time:15:04:05.000} [%{shortfunc}] [%{level}] %{message}`,
)
var log = logging.MustGetLogger("core")

var Node *TextileNode

const roomRepublishInterval = time.Minute
const pingRelayInterval = time.Second * 30
const pingTimeout = 10 * time.Second

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

	// The local raw gateway server
	// Gateway http.Server

	// The local decrypting gateway server
	GatewayProxy *http.Server

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
}

// PhotoList is a JSON-type structure that contains a list of photo hashes
type PhotoList struct {
	Hashes []string `json:"hashes"`
}

// NewNode creates a new TextileNode
func NewNode(repoPath string, centralApiURL string, isMobile bool, logLevel logging.Level) (*TextileNode, error) {
	// shutdown is not clean here yet, so we have to hackily remove
	// the lockfile that should have been removed on shutdown
	// before we start up again
	// TODO: Figure out how to make this work as intended, without doing this
	repoLockFile := filepath.Join(repoPath, lockfile.LockFile)
	os.Remove(repoLockFile)
	dsLockFile := filepath.Join(repoPath, "datastore", "LOCK")
	os.Remove(dsLockFile)

	// log handling
	w := &lumberjack.Logger{
		Filename:   path.Join(repoPath, "logs", "textile.log"),
		MaxSize:    10, // megabytes
		MaxBackups: 3,
		MaxAge:     30, // days
	}
	backendFile := logging.NewLogBackend(w, "", 0)
	backendFileFormatter := logging.NewBackendFormatter(backendFile, fileLogFormat)
	logging.SetBackend(backendFileFormatter)
	logging.SetLevel(logLevel, "")

	// get database handle for wallet indexes
	sqliteDB, err := db.Create(repoPath, "")
	if err != nil {
		return nil, err
	}

	// we may be running in an uninitialized state.
	err = trepo.DoInit(repoPath, isMobile, sqliteDB.Config().Init, sqliteDB.Config().Configure)
	if err != nil && err != trepo.ErrRepoExists {
		return nil, err
	}

	// acquire the repo lock _before_ constructing a node. we need to make
	// sure we are permitted to access the resources (datastore, etc.)
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		log.Errorf("error opening repo: %s", err)
		return nil, err
	}

	// determine the best routing
	var routingOption core.RoutingOption
	if isMobile {
		// TODO: Determine best value for this setting on mobile
		// cfg.Swarm.DisableNatPortMap = true
		routingOption = core.DHTClientOption
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

	// create default servers (no handling until start, uses default addrs)
	// TODO: can we figure out addrs here and now?
	gatewayProxy := &http.Server{Addr: ":9999"}

	// clean central api url
	if centralApiURL[len(centralApiURL)-1:] == "/" {
		centralApiURL = centralApiURL[0 : len(centralApiURL)-1]
	}

	// finally, construct our node
	node := &TextileNode{
		RepoPath:        repoPath,
		Datastore:       sqliteDB,
		GatewayProxy:    gatewayProxy,
		GatewayPassword: ksuid.New().String(),
		LeftRoomChs:     make(map[string]chan struct{}),
		leaveRoomChs:    make(map[string]chan struct{}),
		ipfsConfig:      ncfg,
		isMobile:        isMobile,
		centralUserAPI:  fmt.Sprintf("%s/api/v1/users", centralApiURL),
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

	// TODO: remove this post beta
	ba := node.Datastore.Albums().GetAlbumByName("beta")
	if ba == nil {
		err = node.CreateAlbum("avoid lunch soccer wool stock evil nature nest erase enough leaf blood twenty fence soldier brave forum loyal recycle minor small pencil addict pact", "beta")
		if err != nil {
			log.Errorf("error creating beta album: %s", err)
			return nil, err
		}
	}

	return node, nil
}

// Start the node
func (t *TextileNode) Start() error {
	if t.IpfsNode != nil {
		return ErrNodeRunning
	}

	// raise file descriptor limit
	if err := utilmain.ManageFdLimit(); err != nil {
		log.Errorf("setting file descriptor limit: %s", err)
	}

	// start the ipfs node
	log.Info("starting node...")
	cctx, cancel := context.WithCancel(context.Background())
	t.Cancel = cancel
	nd, err := core.NewNode(cctx, t.ipfsConfig)
	if err != nil {
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
	gwpErrc, err = startGateway(t)
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

	if t.isMobile {
		log.Info("mobile node is ready")
	} else {
		log.Info("desktop node is ready")
	}
	return nil
}

// StartGarbageCollection starts auto garbage cleanup
// TODO: verify this is finding the correct repo, might be using IPFS_PATH
func (t *TextileNode) StartGarbageCollection() (<-chan error, error) {
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

// Stop the node
func (t *TextileNode) Stop() error {
	if t.IpfsNode == nil {
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
		return err
	}

	t.IpfsNode = nil
	return nil
}

// SignUp requests a new username and token from the central api and saves them locally
func (t *TextileNode) SignUp(reg *cmodels.Registration) error {
	// remote signup
	res, err := central.SignUp(reg, t.centralUserAPI)
	if err != nil {
		log.Errorf("signup error: %s", err)
		return err
	}
	if res.Error != nil {
		log.Errorf("signup error from central: %s", err)
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
	// remote signin
	res, err := central.SignIn(creds, t.centralUserAPI)
	if err != nil {
		log.Errorf("signin error: %s", err)
		return err
	}
	if res.Error != nil {
		log.Errorf("signin error from central: %s", err)
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
	// remote is stateless, so we just ditch the local token
	if err := t.Datastore.Config().SignOut(); err != nil {
		log.Errorf("local signout error: %s", err)
		return err
	}
	return nil
}

// JoinRoom with a given id
func (t *TextileNode) JoinRoom(id string, datac chan string) {
	// create the subscription
	sub, err := t.IpfsNode.Floodsub.Subscribe(id)
	if err != nil {
		log.Errorf("error creating subscription: %s", err)
		return
	}
	log.Infof("joined room: %s\n", id)

	t.leaveRoomChs[id] = make(chan struct{})
	t.LeftRoomChs[id] = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	leave := func() {
		cancel()
		close(t.LeftRoomChs[id])

		delete(t.leaveRoomChs, id)
		delete(t.LeftRoomChs, id)
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
				log.Infof(err.Error())
				return
			}

			// handle the update
			if err = t.handleRoomUpdate(msg, id, datac); err != nil {
				log.Errorf("error handling room update: %s", err)
			}
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
	if t.leaveRoomChs[id] == nil {
		return
	}
	close(t.leaveRoomChs[id])
}

// WaitForRoom to join
func (t *TextileNode) WaitForRoom() {
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
				log.Infof(err.Error())
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
			log.Infof("decrypted mnemonic phrase as: %s\n", ps)

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

// ConnectToRoomPeers on a given topic
func (t *TextileNode) ConnectToRoomPeers(topic string) context.CancelFunc {
	blk := blocks.NewBlock([]byte("floodsub:" + topic))
	err := t.IpfsNode.Blocks.AddBlock(blk)
	if err != nil {
		log.Error("pubsub discovery: ", err)
		return nil
	}

	ctx, cancel := context.WithCancel(t.IpfsNode.Context())
	go connectToPubSubPeers(ctx, t.IpfsNode, blk.Cid())

	return cancel
}

// GatewayPort requests the active gateway port
func (t *TextileNode) GatewayPort() (int, error) {
	// Get config and set address to raw gateway address plus one thousand,
	// so a raw gateway on 8182 means this will run on 9182
	cfg, err := t.Context.GetConfig()
	if err != nil {
		log.Errorf("get gateway port failed: %s", err)
		return -1, err
	}
	tmp := strings.Split(cfg.Addresses.Gateway, "/")
	gaddrs := tmp[len(tmp)-1]
	gaddr, err := strconv.ParseInt(gaddrs, 10, 64)
	if err != nil {
		log.Errorf("get address failed: %s", err)
		return -1, err
	}
	port := gaddr + 1000
	return int(port), nil
}

// CreateAlbum creates an album with a given name and mnemonic words
func (t *TextileNode) CreateAlbum(mnemonic string, name string) error {
	// use phrase if provided
	log.Infof("creating a new album: %s", name)
	if mnemonic == "" {
		var err error
		mnemonic, err = createMnemonic(bip39.NewEntropy, bip39.NewMnemonic)
		if err != nil {
			log.Errorf("error creating mnemonic: %s", err)
			return err
		}
		log.Infof("generating %v-bit Ed25519 keypair for: %s", trepo.NBitsForKeypair, name)
	} else {
		log.Infof("regenerating Ed25519 keypair from mnemonic phrase for: %s", name)
	}

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

func (t *TextileNode) AddPhoto(path string, thumb string, album string) (*net.MultipartRequest, error) {
	log.Infof("adding photo %s to %s", path, album)

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
		log.Infof("found last hash: %s", lc)
	}

	// get username
	un, err := t.Datastore.Config().GetUsername()
	if err != nil {
		log.Errorf("username not found (not signed in)")
		un = ""
	}

	// add it
	mr, md, err := photos.Add(t.IpfsNode, a.Key.GetPublic(), p, th, lc, un)
	if err != nil {
		log.Errorf("error adding photo: %s", err)
		return nil, err
	}

	// index
	log.Infof("indexing %s...", mr.Boundary)
	set := &trepo.PhotoSet{
		Cid:      mr.Boundary,
		LastCid:  lc,
		AlbumID:  a.Id,
		MetaData: *md,
		IsLocal:  true,
	}
	err = t.Datastore.Photos().Put(set)
	if err != nil {
		log.Errorf("error indexing photo: %s", err)
		return nil, err
	}

	// publish
	log.Infof("publishing update to %s...", a.Id)
	err = t.IpfsNode.Floodsub.Publish(a.Id, []byte(mr.Boundary))
	if err != nil {
		log.Errorf("error publishing photo update: %s", err)
		return nil, err
	}

	return mr, nil
}

func (t *TextileNode) SharePhoto(hash string, album string) (*net.MultipartRequest, error) {
	log.Infof("sharing photo %s to %s...", hash, album)

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

	return t.AddPhoto(ppath, tpath, album)
}

func (t *TextileNode) GetPhotos(offsetId string, limit int, album string) *PhotoList {
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

	return res
}

// pass in Qm../thumb, or Qm../photo for full image
// album is looked up if not present
func (t *TextileNode) GetFile(path string, album *trepo.PhotoAlbum) ([]byte, error) {
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

func (t *TextileNode) GetMetaData(hash string, album *trepo.PhotoAlbum) (*photos.Metadata, error) {
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

func (t *TextileNode) GetLastHash(hash string, album *trepo.PhotoAlbum) (string, error) {
	b, err := t.GetFile(fmt.Sprintf("%s/last", hash), album)
	if err != nil {
		log.Errorf("error getting last hash file with hash: %s: %s", hash, err)
		return "", err
	}

	return string(b), nil
}

func (t *TextileNode) LoadPhotoAndAlbum(hash string) (*trepo.PhotoSet, *trepo.PhotoAlbum, error) {
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

func (t *TextileNode) UnmarshalPrivatePeerKey() (libp2p.PrivKey, error) {
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

func (t *TextileNode) PingPeer(addrs string, num int, out chan string) error {
	addr, pid, err := parsePeerParam(addrs)
	if addr != nil {
		t.IpfsNode.Peerstore.AddAddr(pid, addr, pstore.TempAddrTTL) // temporary
	}

	if len(t.IpfsNode.Peerstore.Addrs(pid)) == 0 {
		// Make sure we can find the node in question
		log.Infof("looking up peer: %s", pid.Pretty())

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
			log.Infof(msg)
			time.Sleep(time.Second)
		}
	}

	return nil
}

// startGateway starts the secure HTTP gatway server
func startGateway(t *TextileNode) (<-chan error, error) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("SessionId")
		if err != nil || cookie.Value != t.GatewayPassword {
			w.WriteHeader(401)
			return
		}
		log.Infof("valid cookie: %s\n", cookie.Value)
		b, err := t.GetFile(r.URL.Path, nil)
		if err != nil {
			log.Errorf("error decrypting path %s: %s", r.URL.Path, err)
			w.WriteHeader(400)
			return
		}
		w.Write(b)
	})

	port, err := t.GatewayPort()
	if err != nil {
		return nil, err
	}
	portString := fmt.Sprintf(":%d", port)
	// Update address/port
	t.GatewayProxy.Addr = portString

	// Check if the cert files are available.
	certPath := filepath.Join(t.RepoPath, "cert.pem")
	keyPath := filepath.Join(t.RepoPath, "key.pem")
	err = ssl.Check(certPath, keyPath)
	// If they are not available, generate new ones.
	if err != nil {
		err = ssl.Generate(certPath, keyPath, "localhost"+portString)
		if err != nil {
			log.Errorf("failed to create https certs: %s", err)
			return nil, err
		}
	}

	// Start the HTTPS server in a goroutine
	errc := make(chan error)
	go func() {
		errc <- t.GatewayProxy.ListenAndServeTLS(certPath, keyPath)
		close(errc)
	}()
	log.Infof("decrypting gateway (readonly) server listening on /ip4/127.0.0.1/tcp/%d\n", port)

	// NOTE: No need to actually do this, but keeping commented out for testing
	// Start the HTTP server and redirect all incoming connections to HTTPS
	//go http.ListenAndServe(":9193", http.HandlerFunc(redirectToHttps))

	return errc, nil
}

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
		select {
		case <-t.IpfsNode.Context().Done():
			log.Info("republishing stopped")
			return
		}
	}
}

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
		log.Infof("re-publishing %s to %s...", latest, a.Id)
		if err := t.IpfsNode.Floodsub.Publish(a.Id, []byte(latest)); err != nil {
			log.Errorf("error re-publishing update: %s", err)
		}
	}
}

func (t *TextileNode) startPingingRelay() {
	relay := "QmTUvaGZqEu7qJw6DuTyhTgiZmZwdp7qN4FD4FFV3TGhjM"

	// do it once right away
	err := t.PingPeer(relay, 1, make(chan string))
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
			err := t.PingPeer(relay, 1, make(chan string))
			if err != nil {
				log.Errorf("ping relay failed: %s", err)
			}
		}
	}()

	// we can stop when the node stops
	for {
		select {
		case <-t.IpfsNode.Context().Done():
			log.Info("pinging relay stopped")
			return
		}
	}
}

func (t *TextileNode) getDataAtPath(path string) ([]byte, error) {
	// convert string to an ipfs path
	ip, err := coreapi.ParsePath(path)
	if err != nil {
		return nil, err
	}

	api := coreapi.NewCoreAPI(t.IpfsNode)
	r, err := api.Unixfs().Cat(t.IpfsNode.Context(), ip)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return ioutil.ReadAll(r)
}

func (t *TextileNode) handleRoomUpdate(msg *floodsub.Message, aid string, datac chan string) error {
	// unpack message
	from := msg.GetFrom().Pretty()
	hash := string(msg.GetData())
	api := coreapi.NewCoreAPI(t.IpfsNode)
	log.Infof("got update from %s in room %s", from, aid)

	// recurse back in time starting at this hash
	err := t.handleHash(hash, aid, api, datac)
	if err != nil {
		return err
	}

	return nil
}

func (t *TextileNode) handleHash(hash string, aid string, api iface.CoreAPI, datac chan string) error {
	// look up the album
	a := t.Datastore.Albums().GetAlbum(aid)
	if a == nil {
		err := errors.New(fmt.Sprintf("could not find album with id: %s", aid))
		return err
	}

	// first update?
	if hash == "" {
		log.Infof("found genesis update, aborting")
		return nil
	}
	log.Infof("handling update: %s...", hash)

	// convert string to an ipfs path
	ip, err := coreapi.ParsePath(hash)
	if err != nil {
		return err
	}

	// check if we aleady have this hash
	set := t.Datastore.Photos().GetPhoto(hash)
	if set != nil {
		log.Infof("update %s exists, aborting", hash)
		return nil
	}

	// pin it
	log.Infof("pinning %s recursively...", hash)
	err = api.Pin().Add(t.IpfsNode.Context(), ip, api.Pin().WithRecursive(true))
	if err != nil {
		return err
	}

	// unpack data set
	log.Infof("unpacking %s...", hash)
	md, err := t.GetMetaData(hash, a)
	if err != nil {
		return err
	}
	last, err := t.GetLastHash(hash, a)
	if err != nil {
		return err
	}

	// index
	log.Infof("indexing %s...", hash)
	set = &trepo.PhotoSet{
		Cid:      hash,
		LastCid:  last,
		AlbumID:  aid,
		MetaData: *md,
		IsLocal:  false,
	}
	err = t.Datastore.Photos().Put(set)
	if err != nil {
		return err
	}

	// don't block on the send since nobody might be listening
	select {
	case datac <- hash:
	default:
	}

	// check last hash
	return t.handleHash(last, aid, api, datac)
}
