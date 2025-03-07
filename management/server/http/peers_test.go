package http

import (
	"encoding/json"
	"github.com/netbirdio/netbird/management/server/http/api"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/netbirdio/netbird/management/server/jwtclaims"

	"github.com/magiconair/properties/assert"
	"github.com/netbirdio/netbird/management/server"
	"github.com/netbirdio/netbird/management/server/mock_server"
)

func initTestMetaData(peer ...*server.Peer) *Peers {
	return &Peers{
		accountManager: &mock_server.MockAccountManager{
			GetAccountFromTokenFunc: func(claims jwtclaims.AuthorizationClaims) (*server.Account, error) {
				return &server.Account{
					Id:     claims.AccountId,
					Domain: "hotmail.com",
					Peers: map[string]*server.Peer{
						"test_peer": peer[0],
					},
				}, nil
			},
		},
		authAudience: "",
		jwtExtractor: jwtclaims.ClaimsExtractor{
			ExtractClaimsFromRequestContext: func(r *http.Request, authAudiance string) jwtclaims.AuthorizationClaims {
				return jwtclaims.AuthorizationClaims{
					UserId:    "test_user",
					Domain:    "hotmail.com",
					AccountId: "test_id",
				}
			},
		},
	}
}

// Tests the GetPeers endpoint reachable in the route /api/peers
// Use the metadata generated by initTestMetaData() to check for values
func TestGetPeers(t *testing.T) {
	tt := []struct {
		name           string
		expectedStatus int
		requestType    string
		requestPath    string
		requestBody    io.Reader
	}{
		{
			name:           "GetPeersMetaData",
			requestType:    http.MethodGet,
			requestPath:    "/api/peers/",
			expectedStatus: http.StatusOK,
		},
	}

	rr := httptest.NewRecorder()
	peer := &server.Peer{
		Key:      "key",
		SetupKey: "setupkey",
		IP:       net.ParseIP("100.64.0.1"),
		Status:   &server.PeerStatus{},
		Name:     "PeerName",
		Meta: server.PeerSystemMeta{
			Hostname:  "hostname",
			GoOS:      "GoOS",
			Kernel:    "kernel",
			Core:      "core",
			Platform:  "platform",
			OS:        "OS",
			WtVersion: "development",
		},
	}

	p := initTestMetaData(peer)

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.requestType, tc.requestPath, tc.requestBody)

			p.GetPeers(rr, req)

			res := rr.Result()
			defer res.Body.Close()

			if status := rr.Code; status != tc.expectedStatus {
				t.Fatalf("handler returned wrong status code: got %v want %v",
					status, http.StatusOK)
			}

			content, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("I don't know what I expected; %v", err)
			}

			respBody := []*api.Peer{}
			err = json.Unmarshal(content, &respBody)
			if err != nil {
				t.Fatalf("Sent content is not in correct json format; %v", err)
			}

			got := respBody[0]
			assert.Equal(t, got.Name, peer.Name)
			assert.Equal(t, got.Version, peer.Meta.WtVersion)
			assert.Equal(t, got.Ip, peer.IP.String())
			assert.Equal(t, got.Os, "OS core")
		})
	}
}
