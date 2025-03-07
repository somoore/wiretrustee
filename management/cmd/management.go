package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/google/uuid"
	httpapi "github.com/netbirdio/netbird/management/server/http"
	"github.com/netbirdio/netbird/management/server/metrics"
	"github.com/netbirdio/netbird/management/server/telemetry"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/netbirdio/netbird/management/server"
	"github.com/netbirdio/netbird/management/server/idp"
	"github.com/netbirdio/netbird/util"

	"github.com/netbirdio/netbird/encryption"
	mgmtProto "github.com/netbirdio/netbird/management/proto"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// ManagementLegacyPort is the port that was used before by the Management gRPC server.
// It is used for backward compatibility now.
const ManagementLegacyPort = 33073

var (
	mgmtPort                int
	mgmtMetricsPort         int
	mgmtLetsencryptDomain   string
	mgmtSingleAccModeDomain string
	certFile                string
	certKey                 string
	config                  *server.Config

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

	mgmtCmd = &cobra.Command{
		Use:   "management",
		Short: "start NetBird Management Server",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// detect whether user specified a port
			userPort := cmd.Flag("port").Changed

			var err error
			config, err = loadMgmtConfig(mgmtConfig)
			if err != nil {
				return fmt.Errorf("failed reading provided config file: %s: %v", mgmtConfig, err)
			}

			tlsEnabled := false
			if mgmtLetsencryptDomain != "" || (config.HttpConfig.CertFile != "" && config.HttpConfig.CertKey != "") {
				tlsEnabled = true
			}

			if !userPort {
				// different defaults for port when tls enabled/disabled
				if tlsEnabled {
					mgmtPort = 443
				} else {
					mgmtPort = 80
				}
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			flag.Parse()
			err := util.InitLog(logLevel, logFile)
			if err != nil {
				return fmt.Errorf("failed initializing log %v", err)
			}

			err = handleRebrand(cmd)
			if err != nil {
				return fmt.Errorf("failed to migrate files %v", err)
			}

			if _, err = os.Stat(config.Datadir); os.IsNotExist(err) {
				err = os.MkdirAll(config.Datadir, os.ModeDir)
				if err != nil {
					return fmt.Errorf("failed creating datadir: %s: %v", config.Datadir, err)
				}
			}

			store, err := server.NewStore(config.Datadir)
			if err != nil {
				return fmt.Errorf("failed creating Store: %s: %v", config.Datadir, err)
			}
			peersUpdateManager := server.NewPeersUpdateManager()

			appMetrics, err := telemetry.NewDefaultAppMetrics(cmd.Context())
			if err != nil {
				return err
			}
			err = appMetrics.Expose(mgmtMetricsPort, "/metrics")
			if err != nil {
				return err
			}

			var idpManager idp.Manager
			if config.IdpManagerConfig != nil {
				idpManager, err = idp.NewManager(*config.IdpManagerConfig, appMetrics)
				if err != nil {
					return fmt.Errorf("failed retrieving a new idp manager with err: %v", err)
				}
			}

			if disableSingleAccMode {
				mgmtSingleAccModeDomain = ""
			}
			accountManager, err := server.BuildManager(store, peersUpdateManager, idpManager, mgmtSingleAccModeDomain)
			if err != nil {
				return fmt.Errorf("failed to build default manager: %v", err)
			}

			turnManager := server.NewTimeBasedAuthSecretsManager(peersUpdateManager, config.TURNConfig)

			gRPCOpts := []grpc.ServerOption{grpc.KeepaliveEnforcementPolicy(kaep), grpc.KeepaliveParams(kasp)}
			var certManager *autocert.Manager
			var tlsConfig *tls.Config
			tlsEnabled := false
			if config.HttpConfig.LetsEncryptDomain != "" {
				certManager, err = encryption.CreateCertManager(config.Datadir, config.HttpConfig.LetsEncryptDomain)
				if err != nil {
					return fmt.Errorf("failed creating LetsEncrypt cert manager: %v", err)
				}
				transportCredentials := credentials.NewTLS(certManager.TLSConfig())
				gRPCOpts = append(gRPCOpts, grpc.Creds(transportCredentials))
				tlsEnabled = true
			} else if config.HttpConfig.CertFile != "" && config.HttpConfig.CertKey != "" {
				tlsConfig, err = loadTLSConfig(config.HttpConfig.CertFile, config.HttpConfig.CertKey)
				if err != nil {
					log.Errorf("cannot load TLS credentials: %v", err)
					return err
				}
				transportCredentials := credentials.NewTLS(tlsConfig)
				gRPCOpts = append(gRPCOpts, grpc.Creds(transportCredentials))
				tlsEnabled = true
			}

			httpAPIHandler, err := httpapi.APIHandler(accountManager, config.HttpConfig.AuthIssuer,
				config.HttpConfig.AuthAudience, config.HttpConfig.AuthKeysLocation, appMetrics)
			if err != nil {
				return fmt.Errorf("failed creating HTTP API handler: %v", err)
			}

			gRPCAPIHandler := grpc.NewServer(gRPCOpts...)
			srv, err := server.NewServer(config, accountManager, peersUpdateManager, turnManager, appMetrics)
			if err != nil {
				return fmt.Errorf("failed creating gRPC API handler: %v", err)
			}
			mgmtProto.RegisterManagementServiceServer(gRPCAPIHandler, srv)

			installationID, err := getInstallationID(store)
			if err != nil {
				log.Errorf("cannot load TLS credentials: %v", err)
				return err
			}

			fmt.Println("metrics ", disableMetrics)

			if !disableMetrics {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				metricsWorker := metrics.NewWorker(ctx, installationID, store, peersUpdateManager)
				go metricsWorker.Run()
			}

			var compatListener net.Listener
			if mgmtPort != ManagementLegacyPort {
				// The Management gRPC server was running on port 33073 previously. Old agents that are already connected to it
				// are using port 33073. For compatibility purposes we keep running a 2nd gRPC server on port 33073.
				compatListener, err = serveGRPC(gRPCAPIHandler, ManagementLegacyPort)
				if err != nil {
					return err
				}
				log.Infof("running gRPC backward compatibility server: %s", compatListener.Addr().String())
			}

			rootHandler := handlerFunc(gRPCAPIHandler, httpAPIHandler)
			var listener net.Listener
			if certManager != nil {
				// a call to certManager.Listener() always creates a new listener so we do it once
				cml := certManager.Listener()
				if mgmtPort == 443 {
					// CertManager, HTTP and gRPC API all on the same port
					rootHandler = certManager.HTTPHandler(rootHandler)
					listener = cml
				} else {
					listener, err = tls.Listen("tcp", fmt.Sprintf(":%d", mgmtPort), certManager.TLSConfig())
					if err != nil {
						return fmt.Errorf("failed creating TLS listener on port %d: %v", mgmtPort, err)
					}
					log.Infof("running HTTP server (LetsEncrypt challenge handler): %s", cml.Addr().String())
					serveHTTP(cml, certManager.HTTPHandler(nil))
				}
			} else if tlsConfig != nil {
				listener, err = tls.Listen("tcp", fmt.Sprintf(":%d", mgmtPort), tlsConfig)
				if err != nil {
					return fmt.Errorf("failed creating TLS listener on port %d: %v", mgmtPort, err)
				}
			} else {
				listener, err = net.Listen("tcp", fmt.Sprintf(":%d", mgmtPort))
				if err != nil {
					return fmt.Errorf("failed creating TCP listener on port %d: %v", mgmtPort, err)
				}
			}

			log.Infof("running HTTP server and gRPC server on the same port: %s", listener.Addr().String())
			serveGRPCWithHTTP(listener, rootHandler, tlsEnabled)

			SetupCloseHandler()

			<-stopCh
			_ = appMetrics.Close()
			_ = listener.Close()
			if certManager != nil {
				_ = certManager.Listener().Close()
			}
			gRPCAPIHandler.Stop()
			log.Infof("stopped Management Service")

			return nil
		},
	}
)

func notifyStop(msg string) {
	select {
	case stopCh <- 1:
		log.Error(msg)
	default:
		// stop has been already called, nothing to report
	}
}

func getInstallationID(store server.Store) (string, error) {
	installationID := store.GetInstallationID()
	if installationID != "" {
		return installationID, nil
	}

	installationID = strings.ToUpper(uuid.New().String())
	err := store.SaveInstallationID(installationID)
	if err != nil {
		return "", err
	}
	return installationID, nil
}

func serveGRPC(grpcServer *grpc.Server, port int) (net.Listener, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	go func() {
		err := grpcServer.Serve(listener)
		if err != nil {
			notifyStop(fmt.Sprintf("failed running gRPC server on port %d: %v", port, err))
		}
	}()
	return listener, nil
}

func serveHTTP(httpListener net.Listener, handler http.Handler) {
	go func() {
		err := http.Serve(httpListener, handler)
		if err != nil {
			notifyStop(fmt.Sprintf("failed running HTTP server: %v", err))
		}
	}()
}

func serveGRPCWithHTTP(listener net.Listener, handler http.Handler, tlsEnabled bool) {
	go func() {
		var err error
		if tlsEnabled {
			err = http.Serve(listener, handler)
		} else {
			// the following magic is needed to support HTTP2 without TLS
			// and still share a single port between gRPC and HTTP APIs
			h1s := &http.Server{
				Handler: h2c.NewHandler(handler, &http2.Server{}),
			}
			err = h1s.Serve(listener)
		}

		if err != nil {
			select {
			case stopCh <- 1:
				log.Errorf("failed to serve HTTP and gRPC server: %v", err)
			default:
				// stop has been already called, nothing to report
			}
		}
	}()
}

func handlerFunc(gRPCHandler *grpc.Server, httpHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		grpcHeader := strings.HasPrefix(request.Header.Get("Content-Type"), "application/grpc") ||
			strings.HasPrefix(request.Header.Get("Content-Type"), "application/grpc+proto")
		if request.ProtoMajor == 2 && grpcHeader {
			gRPCHandler.ServeHTTP(writer, request)
		} else {
			httpHandler.ServeHTTP(writer, request)
		}
	})
}

func loadMgmtConfig(mgmtConfigPath string) (*server.Config, error) {
	config := &server.Config{}
	_, err := util.ReadJson(mgmtConfigPath, config)
	if err != nil {
		return nil, err
	}
	if mgmtLetsencryptDomain != "" {
		config.HttpConfig.LetsEncryptDomain = mgmtLetsencryptDomain
	}
	if mgmtDataDir != "" {
		config.Datadir = mgmtDataDir
	}

	if certKey != "" && certFile != "" {
		config.HttpConfig.CertFile = certFile
		config.HttpConfig.CertKey = certKey
	}

	oidcEndpoint := config.HttpConfig.OIDCConfigEndpoint
	if oidcEndpoint != "" {
		// if OIDCConfigEndpoint is specified, we can load DeviceAuthEndpoint and TokenEndpoint automatically
		log.Infof("loading OIDC configuration from the provided IDP configuration endpoint %s", oidcEndpoint)
		oidcConfig, err := fetchOIDCConfig(oidcEndpoint)
		if err != nil {
			return nil, err
		}
		log.Infof("loaded OIDC configuration from the provided IDP configuration endpoint: %s", oidcEndpoint)

		log.Infof("overriding HttpConfig.AuthIssuer with a new value %s, previously configured value: %s",
			oidcConfig.Issuer, config.HttpConfig.AuthIssuer)
		config.HttpConfig.AuthIssuer = oidcConfig.Issuer

		log.Infof("overriding HttpConfig.AuthKeysLocation (JWT certs) with a new value %s, previously configured value: %s",
			oidcConfig.JwksURI, config.HttpConfig.AuthKeysLocation)
		config.HttpConfig.AuthKeysLocation = oidcConfig.JwksURI

		if !(config.DeviceAuthorizationFlow == nil || strings.ToLower(config.DeviceAuthorizationFlow.Provider) == string(server.NONE)) {
			log.Infof("overriding DeviceAuthorizationFlow.TokenEndpoint with a new value: %s, previously configured value: %s",
				oidcConfig.TokenEndpoint, config.DeviceAuthorizationFlow.ProviderConfig.TokenEndpoint)
			config.DeviceAuthorizationFlow.ProviderConfig.TokenEndpoint = oidcConfig.TokenEndpoint
			log.Infof("overriding DeviceAuthorizationFlow.DeviceAuthEndpoint with a new value: %s, previously configured value: %s",
				oidcConfig.DeviceAuthEndpoint, config.DeviceAuthorizationFlow.ProviderConfig.DeviceAuthEndpoint)
			config.DeviceAuthorizationFlow.ProviderConfig.DeviceAuthEndpoint = oidcConfig.DeviceAuthEndpoint

			u, err := url.Parse(oidcEndpoint)
			if err != nil {
				return nil, err
			}
			log.Infof("overriding DeviceAuthorizationFlow.ProviderConfig.Domain with a new value: %s, previously configured value: %s",
				u.Host, config.DeviceAuthorizationFlow.ProviderConfig.Domain)
			config.DeviceAuthorizationFlow.ProviderConfig.Domain = u.Host
		}
	}

	return config, err
}

// OIDCConfigResponse used for parsing OIDC config response
type OIDCConfigResponse struct {
	Issuer             string `json:"issuer"`
	TokenEndpoint      string `json:"token_endpoint"`
	DeviceAuthEndpoint string `json:"device_authorization_endpoint"`
	JwksURI            string `json:"jwks_uri"`
}

// fetchOIDCConfig fetches OIDC configuration from the IDP
func fetchOIDCConfig(oidcEndpoint string) (OIDCConfigResponse, error) {

	res, err := http.Get(oidcEndpoint)
	if err != nil {
		return OIDCConfigResponse{}, fmt.Errorf("failed fetching OIDC configuration fro mendpoint %s %v", oidcEndpoint, err)
	}

	defer func() {
		err := res.Body.Close()
		if err != nil {
			log.Debugf("failed closing response body %v", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return OIDCConfigResponse{}, fmt.Errorf("failed reading OIDC configuration response body: %v", err)
	}

	if res.StatusCode != 200 {
		return OIDCConfigResponse{}, fmt.Errorf("OIDC configuration request returned status %d with response: %s",
			res.StatusCode, string(body))
	}

	config := OIDCConfigResponse{}
	err = json.Unmarshal(body, &config)
	if err != nil {
		return OIDCConfigResponse{}, fmt.Errorf("failed unmarshaling OIDC configuration response: %v", err)
	}

	return config, nil

}

func loadTLSConfig(certFile string, certKey string) (*tls.Config, error) {
	// Load server's certificate and private key
	serverCert, err := tls.LoadX509KeyPair(certFile, certKey)
	if err != nil {
		return nil, err
	}

	// NewDefaultAppMetrics the credentials and return it
	config := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.NoClientCert,
		NextProtos: []string{
			"h2", "http/1.1", // enable HTTP/2
		},
	}

	return config, nil
}

func handleRebrand(cmd *cobra.Command) error {
	var err error
	if logFile == defaultLogFile {
		if migrateToNetbird(oldDefaultLogFile, defaultLogFile) {
			cmd.Printf("will copy Log dir %s and its content to %s\n", oldDefaultLogDir, defaultLogDir)
			err = cpDir(oldDefaultLogDir, defaultLogDir)
			if err != nil {
				return err
			}
		}
	}
	if mgmtConfig == defaultMgmtConfig {
		if migrateToNetbird(oldDefaultMgmtConfig, defaultMgmtConfig) {
			cmd.Printf("will copy Config dir %s and its content to %s\n", oldDefaultMgmtConfigDir, defaultMgmtConfigDir)
			err = cpDir(oldDefaultMgmtConfigDir, defaultMgmtConfigDir)
			if err != nil {
				return err
			}
		}
	}
	if mgmtDataDir == defaultMgmtDataDir {
		if migrateToNetbird(oldDefaultMgmtDataDir, defaultMgmtDataDir) {
			cmd.Printf("will copy Config dir %s and its content to %s\n", oldDefaultMgmtDataDir, defaultMgmtDataDir)
			err = cpDir(oldDefaultMgmtDataDir, defaultMgmtDataDir)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func cpFile(src, dst string) error {
	var err error
	var srcfd *os.File
	var dstfd *os.File
	var srcinfo os.FileInfo

	if srcfd, err = os.Open(src); err != nil {
		return err
	}
	defer srcfd.Close()

	if dstfd, err = os.Create(dst); err != nil {
		return err
	}
	defer dstfd.Close()

	if _, err = io.Copy(dstfd, srcfd); err != nil {
		return err
	}
	if srcinfo, err = os.Stat(src); err != nil {
		return err
	}
	return os.Chmod(dst, srcinfo.Mode())
}

func copySymLink(source, dest string) error {
	link, err := os.Readlink(source)
	if err != nil {
		return err
	}
	return os.Symlink(link, dest)
}

func cpDir(src string, dst string) error {
	var err error
	var fds []os.DirEntry
	var srcinfo os.FileInfo

	if srcinfo, err = os.Stat(src); err != nil {
		return err
	}

	if err = os.MkdirAll(dst, srcinfo.Mode()); err != nil {
		return err
	}

	if fds, err = os.ReadDir(src); err != nil {
		return err
	}
	for _, fd := range fds {
		srcfp := path.Join(src, fd.Name())
		dstfp := path.Join(dst, fd.Name())

		fileInfo, err := os.Stat(srcfp)
		if err != nil {
			log.Fatalf("Couldn't get fileInfo; %v", err)
		}

		switch fileInfo.Mode() & os.ModeType {
		case os.ModeSymlink:
			if err = copySymLink(srcfp, dstfp); err != nil {
				log.Fatalf("Failed to copy from %s to %s; %v", srcfp, dstfp, err)
			}
		case os.ModeDir:
			if err = cpDir(srcfp, dstfp); err != nil {
				log.Fatalf("Failed to copy from %s to %s; %v", srcfp, dstfp, err)
			}
		default:
			if err = cpFile(srcfp, dstfp); err != nil {
				log.Fatalf("Failed to copy from %s to %s; %v", srcfp, dstfp, err)
			}
		}
	}
	return nil
}

func migrateToNetbird(oldPath, newPath string) bool {
	_, errOld := os.Stat(oldPath)
	_, errNew := os.Stat(newPath)

	if errors.Is(errOld, fs.ErrNotExist) || errNew == nil {
		return false
	}

	return true
}
