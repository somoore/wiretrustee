package internal

import (
	"context"
	"fmt"
	"github.com/netbirdio/netbird/client/internal/routemanager"
	"github.com/netbirdio/netbird/client/ssh"
	nbstatus "github.com/netbirdio/netbird/client/status"
	"github.com/netbirdio/netbird/iface"
	"github.com/netbirdio/netbird/route"
	"github.com/stretchr/testify/assert"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netbirdio/netbird/client/system"
	mgmt "github.com/netbirdio/netbird/management/client"
	mgmtProto "github.com/netbirdio/netbird/management/proto"
	"github.com/netbirdio/netbird/management/server"
	signal "github.com/netbirdio/netbird/signal/client"
	"github.com/netbirdio/netbird/signal/proto"
	signalServer "github.com/netbirdio/netbird/signal/server"
	"github.com/netbirdio/netbird/util"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

var (
	kaep = keepalive.EnforcementPolicy{
		MinTime:             15 * time.Second,
		PermitWithoutStream: true,
	}

	kasp = keepalive.ServerParameters{
		MaxConnectionIdle:     15 * time.Second,
		MaxConnectionAgeGrace: 5 * time.Second,
		Time:                  5 * time.Second,
		Timeout:               2 * time.Second,
	}
)

func TestEngine_SSH(t *testing.T) {

	if runtime.GOOS == "windows" {
		t.Skip("skipping TestEngine_SSH on Windows")
	}

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := NewEngine(ctx, cancel, &signal.MockClient{}, &mgmt.MockClient{}, &EngineConfig{
		WgIfaceName:  "utun101",
		WgAddr:       "100.64.0.1/24",
		WgPrivateKey: key,
		WgPort:       33100,
	}, nbstatus.NewRecorder())

	var sshKeysAdded []string
	var sshPeersRemoved []string

	sshCtx, cancel := context.WithCancel(context.Background())

	engine.sshServerFunc = func(hostKeyPEM []byte, addr string) (ssh.Server, error) {
		return &ssh.MockServer{
			Ctx: sshCtx,
			StopFunc: func() error {
				cancel()
				return nil
			},
			StartFunc: func() error {
				<-ctx.Done()
				return ctx.Err()
			},
			AddAuthorizedKeyFunc: func(peer, newKey string) error {
				sshKeysAdded = append(sshKeysAdded, newKey)
				return nil
			},
			RemoveAuthorizedKeyFunc: func(peer string) {
				sshPeersRemoved = append(sshPeersRemoved, peer)
			},
		}, nil
	}
	err = engine.Start()
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		err := engine.Stop()
		if err != nil {
			return
		}
	}()

	peerWithSSH := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "MNHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		AllowedIps: []string{"100.64.0.21/24"},
		SshConfig: &mgmtProto.SSHConfig{
			SshPubKey: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFATYCqaQw/9id1Qkq3n16JYhDhXraI6Pc1fgB8ynEfQ"),
		},
	}

	// SSH server is not enabled so SSH config of a remote peer should be ignored
	networkMap := &mgmtProto.NetworkMap{
		Serial:             6,
		PeerConfig:         nil,
		RemotePeers:        []*mgmtProto.RemotePeerConfig{peerWithSSH},
		RemotePeersIsEmpty: false,
	}

	err = engine.updateNetworkMap(networkMap)
	if err != nil {
		t.Fatal(err)
	}

	assert.Nil(t, engine.sshServer)

	// SSH server is enabled, therefore SSH config should be applied
	networkMap = &mgmtProto.NetworkMap{
		Serial: 7,
		PeerConfig: &mgmtProto.PeerConfig{Address: "100.64.0.1/24",
			SshConfig: &mgmtProto.SSHConfig{SshEnabled: true}},
		RemotePeers:        []*mgmtProto.RemotePeerConfig{peerWithSSH},
		RemotePeersIsEmpty: false,
	}

	err = engine.updateNetworkMap(networkMap)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(250 * time.Millisecond)
	assert.NotNil(t, engine.sshServer)
	assert.Contains(t, sshKeysAdded, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFATYCqaQw/9id1Qkq3n16JYhDhXraI6Pc1fgB8ynEfQ")

	// now remove peer
	networkMap = &mgmtProto.NetworkMap{
		Serial:             8,
		RemotePeers:        []*mgmtProto.RemotePeerConfig{},
		RemotePeersIsEmpty: false,
	}

	err = engine.updateNetworkMap(networkMap)
	if err != nil {
		t.Fatal(err)
	}

	//time.Sleep(250 * time.Millisecond)
	assert.NotNil(t, engine.sshServer)
	assert.Contains(t, sshPeersRemoved, "MNHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=")

	// now disable SSH server
	networkMap = &mgmtProto.NetworkMap{
		Serial: 9,
		PeerConfig: &mgmtProto.PeerConfig{Address: "100.64.0.1/24",
			SshConfig: &mgmtProto.SSHConfig{SshEnabled: false}},
		RemotePeers:        []*mgmtProto.RemotePeerConfig{peerWithSSH},
		RemotePeersIsEmpty: false,
	}

	err = engine.updateNetworkMap(networkMap)
	if err != nil {
		t.Fatal(err)
	}

	assert.Nil(t, engine.sshServer)

}

func TestEngine_UpdateNetworkMap(t *testing.T) {
	// test setup
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := NewEngine(ctx, cancel, &signal.MockClient{}, &mgmt.MockClient{}, &EngineConfig{
		WgIfaceName:  "utun102",
		WgAddr:       "100.64.0.1/24",
		WgPrivateKey: key,
		WgPort:       33100,
	}, nbstatus.NewRecorder())
	engine.wgInterface, err = iface.NewWGIFace("utun102", "100.64.0.1/24", iface.DefaultMTU)
	engine.routeManager = routemanager.NewManager(ctx, key.PublicKey().String(), engine.wgInterface, engine.statusRecorder)

	type testCase struct {
		name       string
		networkMap *mgmtProto.NetworkMap

		expectedLen    int
		expectedPeers  []*mgmtProto.RemotePeerConfig
		expectedSerial uint64
	}

	peer1 := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		AllowedIps: []string{"100.64.0.10/24"},
	}

	peer2 := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "LLHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		AllowedIps: []string{"100.64.0.11/24"},
	}

	peer3 := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "GGHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		AllowedIps: []string{"100.64.0.12/24"},
	}

	modifiedPeer3 := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "GGHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		AllowedIps: []string{"100.64.0.20/24"},
	}

	case1 := testCase{
		name: "input with a new peer to add",
		networkMap: &mgmtProto.NetworkMap{
			Serial:     1,
			PeerConfig: nil,
			RemotePeers: []*mgmtProto.RemotePeerConfig{
				peer1,
			},
			RemotePeersIsEmpty: false,
		},
		expectedLen:    1,
		expectedPeers:  []*mgmtProto.RemotePeerConfig{peer1},
		expectedSerial: 1,
	}

	// 2nd case - one extra peer added and network map has CurrentSerial grater than local => apply the update
	case2 := testCase{
		name: "input with an old peer and a new peer to add",
		networkMap: &mgmtProto.NetworkMap{
			Serial:     2,
			PeerConfig: nil,
			RemotePeers: []*mgmtProto.RemotePeerConfig{
				peer1, peer2,
			},
			RemotePeersIsEmpty: false,
		},
		expectedLen:    2,
		expectedPeers:  []*mgmtProto.RemotePeerConfig{peer1, peer2},
		expectedSerial: 2,
	}

	case3 := testCase{
		name: "input with outdated (old) update to ignore",
		networkMap: &mgmtProto.NetworkMap{
			Serial:     0,
			PeerConfig: nil,
			RemotePeers: []*mgmtProto.RemotePeerConfig{
				peer1, peer2, peer3,
			},
			RemotePeersIsEmpty: false,
		},
		expectedLen:    2,
		expectedPeers:  []*mgmtProto.RemotePeerConfig{peer1, peer2},
		expectedSerial: 2,
	}

	case4 := testCase{
		name: "input with one peer to remove and one new to add",
		networkMap: &mgmtProto.NetworkMap{
			Serial:     4,
			PeerConfig: nil,
			RemotePeers: []*mgmtProto.RemotePeerConfig{
				peer2, peer3,
			},
			RemotePeersIsEmpty: false,
		},
		expectedLen:    2,
		expectedPeers:  []*mgmtProto.RemotePeerConfig{peer2, peer3},
		expectedSerial: 4,
	}

	case5 := testCase{
		name: "input with one peer to modify",
		networkMap: &mgmtProto.NetworkMap{
			Serial:     4,
			PeerConfig: nil,
			RemotePeers: []*mgmtProto.RemotePeerConfig{
				modifiedPeer3, peer2,
			},
			RemotePeersIsEmpty: false,
		},
		expectedLen:    2,
		expectedPeers:  []*mgmtProto.RemotePeerConfig{peer2, modifiedPeer3},
		expectedSerial: 4,
	}

	case6 := testCase{
		name: "input with all peers to remove",
		networkMap: &mgmtProto.NetworkMap{
			Serial:             5,
			PeerConfig:         nil,
			RemotePeers:        []*mgmtProto.RemotePeerConfig{},
			RemotePeersIsEmpty: true,
		},
		expectedLen:    0,
		expectedPeers:  nil,
		expectedSerial: 5,
	}

	for _, c := range []testCase{case1, case2, case3, case4, case5, case6} {
		t.Run(c.name, func(t *testing.T) {
			err = engine.updateNetworkMap(c.networkMap)
			if err != nil {
				t.Fatal(err)
				return
			}

			if len(engine.peerConns) != c.expectedLen {
				t.Errorf("expecting Engine.peerConns to be of size %d, got %d", c.expectedLen, len(engine.peerConns))
			}

			if engine.networkSerial != c.expectedSerial {
				t.Errorf("expecting Engine.networkSerial to be equal to %d, actual %d", c.expectedSerial, engine.networkSerial)
			}

			for _, p := range c.expectedPeers {
				conn, ok := engine.peerConns[p.GetWgPubKey()]
				if !ok {
					t.Errorf("expecting Engine.peerConns to contain peer %s", p)
				}
				expectedAllowedIPs := strings.Join(p.AllowedIps, ",")
				if conn.GetConf().ProxyConfig.AllowedIps != expectedAllowedIPs {
					t.Errorf("expecting peer %s to have AllowedIPs= %s, got %s", p.GetWgPubKey(),
						expectedAllowedIPs, conn.GetConf().ProxyConfig.AllowedIps)
				}
			}
		})
	}
}

func TestEngine_Sync(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// feed updates to Engine via mocked Management client
	updates := make(chan *mgmtProto.SyncResponse)
	defer close(updates)
	syncFunc := func(msgHandler func(msg *mgmtProto.SyncResponse) error) error {
		for msg := range updates {
			err := msgHandler(msg)
			if err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}

	engine := NewEngine(ctx, cancel, &signal.MockClient{}, &mgmt.MockClient{SyncFunc: syncFunc}, &EngineConfig{
		WgIfaceName:  "utun103",
		WgAddr:       "100.64.0.1/24",
		WgPrivateKey: key,
		WgPort:       33100,
	}, nbstatus.NewRecorder())

	defer func() {
		err := engine.Stop()
		if err != nil {
			return
		}
	}()

	err = engine.Start()
	if err != nil {
		t.Fatal(err)
		return
	}

	peer1 := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		AllowedIps: []string{"100.64.0.10/24"},
	}
	peer2 := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "LLHf3Ma6z6mdLbriAJbqhX9+nM/B71lgw2+91q3LlhU=",
		AllowedIps: []string{"100.64.0.11/24"},
	}
	peer3 := &mgmtProto.RemotePeerConfig{
		WgPubKey:   "GGHf3Ma6z6mdLbriAJbqhX9+nM/B71lgw2+91q3LlhU=",
		AllowedIps: []string{"100.64.0.12/24"},
	}
	// 1st update with just 1 peer and serial larger than the current serial of the engine => apply update
	updates <- &mgmtProto.SyncResponse{
		NetworkMap: &mgmtProto.NetworkMap{
			Serial:             10,
			PeerConfig:         nil,
			RemotePeers:        []*mgmtProto.RemotePeerConfig{peer1, peer2, peer3},
			RemotePeersIsEmpty: false,
		},
	}

	timeout := time.After(time.Second * 2)
	for {
		select {
		case <-timeout:
			t.Fatalf("timeout while waiting for test to finish")
			return
		default:
		}

		if len(engine.GetPeers()) == 3 && engine.networkSerial == 10 {
			break
		}
	}
}

func TestEngine_UpdateNetworkMapWithRoutes(t *testing.T) {
	testCases := []struct {
		name           string
		inputErr       error
		networkMap     *mgmtProto.NetworkMap
		expectedLen    int
		expectedRoutes []*route.Route
		expectedSerial uint64
	}{
		{
			name: "Routes Update Should Be Passed To Manager",
			networkMap: &mgmtProto.NetworkMap{
				Serial:             1,
				PeerConfig:         nil,
				RemotePeersIsEmpty: false,
				Routes: []*mgmtProto.Route{
					{
						ID:          "a",
						Network:     "192.168.0.0/24",
						NetID:       "n1",
						Peer:        "p1",
						NetworkType: 1,
						Masquerade:  false,
					},
					{
						ID:          "b",
						Network:     "192.168.1.0/24",
						NetID:       "n2",
						Peer:        "p1",
						NetworkType: 1,
						Masquerade:  false,
					},
				},
			},
			expectedLen: 2,
			expectedRoutes: []*route.Route{
				{
					ID:          "a",
					Network:     netip.MustParsePrefix("192.168.0.0/24"),
					NetID:       "n1",
					Peer:        "p1",
					NetworkType: 1,
					Masquerade:  false,
				},
				{
					ID:          "b",
					Network:     netip.MustParsePrefix("192.168.1.0/24"),
					NetID:       "n2",
					Peer:        "p1",
					NetworkType: 1,
					Masquerade:  false,
				},
			},
			expectedSerial: 1,
		},
		{
			name: "Empty Routes Update Should Be Passed",
			networkMap: &mgmtProto.NetworkMap{
				Serial:             1,
				PeerConfig:         nil,
				RemotePeersIsEmpty: false,
				Routes:             nil,
			},
			expectedLen:    0,
			expectedRoutes: []*route.Route{},
			expectedSerial: 1,
		},
		{
			name:     "Error Shouldn't Break Engine",
			inputErr: fmt.Errorf("mocking error"),
			networkMap: &mgmtProto.NetworkMap{
				Serial:             1,
				PeerConfig:         nil,
				RemotePeersIsEmpty: false,
				Routes:             nil,
			},
			expectedLen:    0,
			expectedRoutes: []*route.Route{},
			expectedSerial: 1,
		},
	}

	for n, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// test setup
			key, err := wgtypes.GeneratePrivateKey()
			if err != nil {
				t.Fatal(err)
				return
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			wgIfaceName := fmt.Sprintf("utun%d", 104+n)
			wgAddr := fmt.Sprintf("100.66.%d.1/24", n)

			engine := NewEngine(ctx, cancel, &signal.MockClient{}, &mgmt.MockClient{}, &EngineConfig{
				WgIfaceName:  wgIfaceName,
				WgAddr:       wgAddr,
				WgPrivateKey: key,
				WgPort:       33100,
			}, nbstatus.NewRecorder())
			engine.wgInterface, err = iface.NewWGIFace(wgIfaceName, wgAddr, iface.DefaultMTU)
			assert.NoError(t, err, "shouldn't return error")
			input := struct {
				inputSerial uint64
				inputRoutes []*route.Route
			}{}

			mockRouteManager := &routemanager.MockManager{
				UpdateRoutesFunc: func(updateSerial uint64, newRoutes []*route.Route) error {
					input.inputSerial = updateSerial
					input.inputRoutes = newRoutes
					return testCase.inputErr
				},
			}

			engine.routeManager = mockRouteManager

			defer func() {
				exitErr := engine.Stop()
				if exitErr != nil {
					return
				}
			}()

			err = engine.updateNetworkMap(testCase.networkMap)
			assert.NoError(t, err, "shouldn't return error")
			assert.Equal(t, testCase.expectedSerial, input.inputSerial, "serial should match")
			assert.Len(t, input.inputRoutes, testCase.expectedLen, "routes len should match")
			assert.Equal(t, testCase.expectedRoutes, input.inputRoutes, "routes should match")
		})
	}
}

func TestEngine_MultiplePeers(t *testing.T) {
	// log.SetLevel(log.DebugLevel)

	dir := t.TempDir()

	err := util.CopyFileContents("../testdata/store.json", filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err = os.Remove(filepath.Join(dir, "store.json")) //nolint
		if err != nil {
			t.Fatal(err)
			return
		}
	}()

	ctx, cancel := context.WithCancel(CtxInitState(context.Background()))
	defer cancel()

	sport := 10010
	sigServer, err := startSignal(sport)
	if err != nil {
		t.Fatal(err)
		return
	}
	defer sigServer.Stop()
	mport := 33081
	mgmtServer, err := startManagement(mport, dir)
	if err != nil {
		t.Fatal(err)
		return
	}
	defer mgmtServer.GracefulStop()

	setupKey := "A2C8E62B-38F5-4553-B31E-DD66C696CEBB"

	mu := sync.Mutex{}
	engines := []*Engine{}
	numPeers := 10
	wg := sync.WaitGroup{}
	wg.Add(numPeers)
	// create and start peers
	for i := 0; i < numPeers; i++ {
		j := i
		go func() {
			engine, err := createEngine(ctx, cancel, setupKey, j, mport, sport)
			if err != nil {
				wg.Done()
				t.Errorf("unable to create the engine for peer %d with error %v", j, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			err = engine.Start()
			if err != nil {
				t.Errorf("unable to start engine for peer %d with error %v", j, err)
				wg.Done()
				return
			}
			engines = append(engines, engine)
			wg.Done()
		}()
	}

	// wait until all have been created and started
	wg.Wait()
	if len(engines) != numPeers {
		t.Fatal("not all peers was started")
	}
	// check whether all the peer have expected peers connected

	expectedConnected := numPeers * (numPeers - 1)

	// adjust according to timeouts
	timeout := 50 * time.Second
	timeoutChan := time.After(timeout)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-timeoutChan:
			t.Fatalf("waiting for expected connections timeout after %s", timeout.String())
			break loop
		case <-ticker.C:
			totalConnected := 0
			for _, engine := range engines {
				totalConnected = totalConnected + len(engine.GetConnectedPeers())
			}
			if totalConnected == expectedConnected {
				log.Infof("total connected=%d", totalConnected)
				break loop
			}
			log.Infof("total connected=%d", totalConnected)
		}
	}
	// cleanup test
	for n, peerEngine := range engines {
		t.Logf("stopping peer with interface %s from multipeer test, loopIndex %d", peerEngine.wgInterface.Name, n)
		errStop := peerEngine.mgmClient.Close()
		if errStop != nil {
			log.Infoln("got error trying to close management clients from engine: ", errStop)
		}
		errStop = peerEngine.Stop()
		if errStop != nil {
			log.Infoln("got error trying to close testing peers engine: ", errStop)
		}
	}
}

func createEngine(ctx context.Context, cancel context.CancelFunc, setupKey string, i int, mport int, sport int) (*Engine, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	mgmtClient, err := mgmt.NewClient(ctx, fmt.Sprintf("localhost:%d", mport), key, false)
	if err != nil {
		return nil, err
	}
	signalClient, err := signal.NewClient(ctx, fmt.Sprintf("localhost:%d", sport), key, false)
	if err != nil {
		return nil, err
	}

	publicKey, err := mgmtClient.GetServerPublicKey()
	if err != nil {
		return nil, err
	}

	info := system.GetInfo(ctx)
	resp, err := mgmtClient.Register(*publicKey, setupKey, "", info, nil)
	if err != nil {
		return nil, err
	}

	var ifaceName string
	if runtime.GOOS == "darwin" {
		ifaceName = fmt.Sprintf("utun1%d", i)
	} else {
		ifaceName = fmt.Sprintf("wt%d", i)
	}

	wgPort := 33100 + i
	conf := &EngineConfig{
		WgIfaceName:  ifaceName,
		WgAddr:       resp.PeerConfig.Address,
		WgPrivateKey: key,
		WgPort:       wgPort,
	}

	return NewEngine(ctx, cancel, signalClient, mgmtClient, conf, nbstatus.NewRecorder()), nil
}

func startSignal(port int) (*grpc.Server, error) {
	s := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(kaep), grpc.KeepaliveParams(kasp))

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	proto.RegisterSignalExchangeServer(s, signalServer.NewServer())

	go func() {
		if err = s.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	return s, nil
}

func startManagement(port int, dataDir string) (*grpc.Server, error) {
	config := &server.Config{
		Stuns:      []*server.Host{},
		TURNConfig: &server.TURNConfig{},
		Signal: &server.Host{
			Proto: "http",
			URI:   "localhost:10000",
		},
		Datadir:    dataDir,
		HttpConfig: nil,
	}

	lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return nil, err
	}
	s := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(kaep), grpc.KeepaliveParams(kasp))
	store, err := server.NewStore(config.Datadir)
	if err != nil {
		log.Fatalf("failed creating a store: %s: %v", config.Datadir, err)
	}
	peersUpdateManager := server.NewPeersUpdateManager()
	accountManager, err := server.BuildManager(store, peersUpdateManager, nil, "")
	if err != nil {
		return nil, err
	}
	turnManager := server.NewTimeBasedAuthSecretsManager(peersUpdateManager, config.TURNConfig)
	mgmtServer, err := server.NewServer(config, accountManager, peersUpdateManager, turnManager, nil)
	if err != nil {
		return nil, err
	}
	mgmtProto.RegisterManagementServiceServer(s, mgmtServer)
	go func() {
		if err = s.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	return s, nil
}
