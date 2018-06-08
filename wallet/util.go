package wallet

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"github.com/textileio/textile-go/crypto"
	iaddr "gx/ipfs/QmQViVWBHbU6HmYjXcdNq7tVASCNgdg64ZGcauuDkLCivW/go-ipfs-addr"
	"gx/ipfs/QmTjNRVt2fvaRFu93keEC7z5M1GS1iH6qZ9227htQioTUY/go-ipfs-cmds"
	ma "gx/ipfs/QmWWQ2Txc2c6tqjsBpzg5Ar652cHPGNsQQp2SejkNmkUMb/go-multiaddr"
	pstore "gx/ipfs/QmXauCuJzmzapetmC6W4TuDJLL1yFFrVzSHoWv8YdbmnxH/go-libp2p-peerstore"
	"gx/ipfs/QmZoWKhxUmZ2seW4BzX6fJkNR8hh9PsGModr7q171yq2SS/go-libp2p-peer"
	libp2p "gx/ipfs/QmaPbCnUMBohSGo3KnxEa2bHqyJVVeEEcwtqJAYxerieBo/go-libp2p-crypto"
	"gx/ipfs/QmcKwjeebv5SX3VFUGDFa4BNMYhy14RRaCzQP7JN3UQDpB/go-ipfs/core"
	"gx/ipfs/QmcKwjeebv5SX3VFUGDFa4BNMYhy14RRaCzQP7JN3UQDpB/go-ipfs/core/coreapi"
	"gx/ipfs/QmcKwjeebv5SX3VFUGDFa4BNMYhy14RRaCzQP7JN3UQDpB/go-ipfs/core/coreapi/interface/options"
	"gx/ipfs/QmcKwjeebv5SX3VFUGDFa4BNMYhy14RRaCzQP7JN3UQDpB/go-ipfs/core/coreunix"
	uio "gx/ipfs/QmcKwjeebv5SX3VFUGDFa4BNMYhy14RRaCzQP7JN3UQDpB/go-ipfs/unixfs/io"
	"gx/ipfs/QmcZfnkapfECQGcLZaf9B79NRg7cRa9EnZh4LSbkCzwNvY/go-cid"
	ipld "gx/ipfs/Qme5bWv7wtjUNGsK2BNGVUFPKiuxWrsqrtvYwCLRw8YFES/go-ipld-format"
	"io"
	"io/ioutil"
	"sort"
	"strings"
	"time"
)

// GetDataAtPath cats any data under an ipfs path
func GetDataAtPath(ipfs *core.IpfsNode, path string) ([]byte, error) {
	// convert string to an ipfs path
	ip, err := coreapi.ParsePath(path)
	if err != nil {
		return nil, err
	}

	api := coreapi.NewCoreAPI(ipfs)
	ctx, cancel := context.WithTimeout(ipfs.Context(), catTimeout)
	defer cancel()
	r, err := api.Unixfs().Cat(ctx, ip)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	defer func() {
		if recover() != nil {
			log.Debug("node stopped")
		}
	}()

	return ioutil.ReadAll(r)
}

// createMnemonic creates a new mnemonic phrase with given entropy
func createMnemonic(newEntropy func(int) ([]byte, error), newMnemonic func([]byte) (string, error)) (string, error) {
	entropy, err := newEntropy(256)
	if err != nil {
		return "", err
	}
	mnemonic, err := newMnemonic(entropy)
	if err != nil {
		return "", err
	}
	return mnemonic, nil
}

// identityKeyFromSeed returns a new key identity from a seed
func identityKeyFromSeed(seed []byte) ([]byte, error) {
	hm := hmac.New(sha256.New, []byte("scythian horde"))
	hm.Write(seed)
	reader := bytes.NewReader(hm.Sum(nil))
	// bits are not meaningful w/ this method in ed25519, so specify whatever
	sk, _, err := libp2p.GenerateKeyPairWithReader(libp2p.Ed25519, 2048, reader)
	if err != nil {
		return nil, err
	}
	encodedKey, err := sk.Bytes()
	if err != nil {
		return nil, err
	}
	return encodedKey, nil
}

// printSwarmAddrs prints the addresses of the host
func printSwarmAddrs(node *core.IpfsNode) error {
	if !node.OnlineMode() {
		return ErrOffline
	}

	var lisAddrs []string
	ifaceAddrs, err := node.PeerHost.Network().InterfaceListenAddresses()
	if err != nil {
		return err
	}
	for _, addr := range ifaceAddrs {
		lisAddrs = append(lisAddrs, addr.String())
	}
	sort.Sort(sort.StringSlice(lisAddrs))
	for _, addr := range lisAddrs {
		log.Infof("swarm listening on %s\n", addr)
	}

	var addrs []string
	for _, addr := range node.PeerHost.Addrs() {
		addrs = append(addrs, addr.String())
	}
	sort.Sort(sort.StringSlice(addrs))
	for _, addr := range addrs {
		log.Infof("swarm announcing %s\n", addr)
	}

	return nil
}

// parsePeerParam takes a peer address string and returns p2p params
func parsePeerParam(text string) (ma.Multiaddr, peer.ID, error) {
	// to be replaced with just multiaddr parsing, once ptp is a multiaddr protocol
	idx := strings.LastIndex(text, "/")
	if idx == -1 {
		pid, err := peer.IDB58Decode(text)
		if err != nil {
			return nil, "", err
		}

		return nil, pid, nil
	}

	addrS := text[:idx]
	peeridS := text[idx+1:]

	var maddr ma.Multiaddr
	var pid peer.ID

	// make sure addrS parses as a multiaddr.
	if len(addrS) > 0 {
		var err error
		maddr, err = ma.NewMultiaddr(addrS)
		if err != nil {
			return nil, "", err
		}
	}

	// make sure idS parses as a peer.ID
	var err error
	pid, err = peer.IDB58Decode(peeridS)
	if err != nil {
		return nil, "", err
	}

	return maddr, pid, nil
}

// parseAddresses is a function that takes in a slice of string peer addresses
// (multiaddr + peerid) and returns slices of multiaddrs and peerids.
func parseAddresses(addrs []string) (iaddrs []iaddr.IPFSAddr, err error) {
	iaddrs = make([]iaddr.IPFSAddr, len(addrs))
	for i, saddr := range addrs {
		iaddrs[i], err = iaddr.ParseString(saddr)
		if err != nil {
			return nil, cmds.ClientError("invalid peer address: " + err.Error())
		}
	}
	return
}

// peersWithAddresses is a function that takes in a slice of string peer addresses
// (multiaddr + peerid) and returns a slice of properly constructed peers
func peersWithAddresses(addrs []string) (pis []pstore.PeerInfo, err error) {
	iaddrs, err := parseAddresses(addrs)
	if err != nil {
		return nil, err
	}

	for _, a := range iaddrs {
		pis = append(pis, pstore.PeerInfo{
			ID:    a.ID(),
			Addrs: []ma.Multiaddr{a.Transport()},
		})
	}
	return pis, nil
}

// getEncryptedReaderBytes reads reader bytes and returns the encrypted result
func getEncryptedReaderBytes(reader io.Reader, key []byte) ([]byte, error) {
	bts, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return crypto.EncryptAES(bts, key)
}

// addFileToDirectory adds bytes as file to a virtual directory (dag) structure
func addFileToDirectory(ipfs *core.IpfsNode, dirb *uio.Directory, bts []byte, fname string) error {
	reader := bytes.NewReader(bts)
	str, err := coreunix.Add(ipfs, reader)
	if err != nil {
		return err
	}
	id, err := cid.Decode(str)
	if err != nil {
		return err
	}
	node, err := ipfs.DAG.Get(ipfs.Context(), id)
	if err != nil {
		return err
	}
	if err := dirb.AddChild(ipfs.Context(), fname, node); err != nil {
		return err
	}
	return nil
}

// pinPath takes an ipfs path string and pins it
func pinPath(ipfs *core.IpfsNode, path string, recursive bool) error {
	ip, err := coreapi.ParsePath(path)
	if err != nil {
		log.Errorf("error pinning path: %s, recursive: %t: %s", path, recursive, err)
		return err
	}
	ctx, cancel := context.WithTimeout(ipfs.Context(), pinTimeout)
	defer cancel()
	api := coreapi.NewCoreAPI(ipfs)
	if err := api.Pin().Add(ctx, ip, options.Pin.Recursive(recursive)); err != nil {
		return err
	}
	defer func() {
		if recover() != nil {
			log.Debug("node stopped")
		}
	}()
	return nil
}

// pinDirectory pins a directory exluding any provided links
func pinDirectory(ipfs *core.IpfsNode, dir ipld.Node, exclude []string) error {
	if err := ipfs.Pinning.Pin(ipfs.Context(), dir, false); err != nil {
		return err
	}
outer:
	for _, item := range dir.Links() {
		for _, ex := range exclude {
			if item.Name == ex {
				continue outer
			}
		}
		node, err := item.GetNode(ipfs.Context(), ipfs.DAG)
		if err != nil {
			return err
		}
		ipfs.Pinning.Pin(ipfs.Context(), node, false)
	}
	return ipfs.Pinning.Flush()
}

func getNowBytes() []byte {
	time.Now().Unix()
}
