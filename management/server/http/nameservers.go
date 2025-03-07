package http

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	nbdns "github.com/netbirdio/netbird/dns"
	"github.com/netbirdio/netbird/management/server"
	"github.com/netbirdio/netbird/management/server/http/api"
	"github.com/netbirdio/netbird/management/server/jwtclaims"
	log "github.com/sirupsen/logrus"
	"net/http"
)

// Nameservers is the nameserver group handler of the account
type Nameservers struct {
	jwtExtractor   jwtclaims.ClaimsExtractor
	accountManager server.AccountManager
	authAudience   string
}

// NewNameservers returns a new instance of Nameservers handler
func NewNameservers(accountManager server.AccountManager, authAudience string) *Nameservers {
	return &Nameservers{
		accountManager: accountManager,
		authAudience:   authAudience,
		jwtExtractor:   *jwtclaims.NewClaimsExtractor(nil),
	}
}

// GetAllNameserversHandler returns the list of nameserver groups for the account
func (h *Nameservers) GetAllNameserversHandler(w http.ResponseWriter, r *http.Request) {
	account, err := getJWTAccount(h.accountManager, h.jwtExtractor, h.authAudience, r)
	if err != nil {
		log.Error(err)
		http.Redirect(w, r, "/", http.StatusInternalServerError)
		return
	}

	nsGroups, err := h.accountManager.ListNameServerGroups(account.Id)
	if err != nil {
		toHTTPError(err, w)
		return
	}

	apiNameservers := make([]*api.NameserverGroup, 0)
	for _, r := range nsGroups {
		apiNameservers = append(apiNameservers, toNameserverGroupResponse(r))
	}

	writeJSONObject(w, apiNameservers)
}

// CreateNameserverGroupHandler handles nameserver group creation request
func (h *Nameservers) CreateNameserverGroupHandler(w http.ResponseWriter, r *http.Request) {
	account, err := getJWTAccount(h.accountManager, h.jwtExtractor, h.authAudience, r)
	if err != nil {
		log.Error(err)
		http.Redirect(w, r, "/", http.StatusInternalServerError)
		return
	}
	var req api.PostApiDnsNameserversJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	nsList, err := toServerNSList(req.Nameservers)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	nsGroup, err := h.accountManager.CreateNameServerGroup(account.Id, req.Name, req.Description, nsList, req.Groups, req.Primary, req.Domains, req.Enabled)
	if err != nil {
		toHTTPError(err, w)
		return
	}

	resp := toNameserverGroupResponse(nsGroup)

	writeJSONObject(w, &resp)
}

// UpdateNameserverGroupHandler handles update to a nameserver group identified by a given ID
func (h *Nameservers) UpdateNameserverGroupHandler(w http.ResponseWriter, r *http.Request) {
	account, err := getJWTAccount(h.accountManager, h.jwtExtractor, h.authAudience, r)
	if err != nil {
		log.Error(err)
		http.Redirect(w, r, "/", http.StatusInternalServerError)
		return
	}

	nsGroupID := mux.Vars(r)["id"]
	if len(nsGroupID) == 0 {
		http.Error(w, "invalid nameserver group ID", http.StatusBadRequest)
		return
	}

	var req api.PutApiDnsNameserversIdJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	nsList, err := toServerNSList(req.Nameservers)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	updatedNSGroup := &nbdns.NameServerGroup{
		ID:          nsGroupID,
		Name:        req.Name,
		Description: req.Description,
		NameServers: nsList,
		Groups:      req.Groups,
		Enabled:     req.Enabled,
	}

	err = h.accountManager.SaveNameServerGroup(account.Id, updatedNSGroup)
	if err != nil {
		toHTTPError(err, w)
		return
	}

	resp := toNameserverGroupResponse(updatedNSGroup)

	writeJSONObject(w, &resp)
}

// PatchNameserverGroupHandler handles patch updates to a nameserver group identified by a given ID
func (h *Nameservers) PatchNameserverGroupHandler(w http.ResponseWriter, r *http.Request) {
	account, err := getJWTAccount(h.accountManager, h.jwtExtractor, h.authAudience, r)
	if err != nil {
		log.Error(err)
		http.Redirect(w, r, "/", http.StatusInternalServerError)
		return
	}

	nsGroupID := mux.Vars(r)["id"]
	if len(nsGroupID) == 0 {
		http.Error(w, "invalid nameserver group ID", http.StatusBadRequest)
		return
	}

	var req api.PatchApiDnsNameserversIdJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var operations []server.NameServerGroupUpdateOperation
	for _, patch := range req {
		if patch.Op != api.NameserverGroupPatchOperationOpReplace {
			http.Error(w, fmt.Sprintf("nameserver groups only accepts replace operations, got %s", patch.Op),
				http.StatusBadRequest)
			return
		}
		switch patch.Path {
		case api.NameserverGroupPatchOperationPathName:
			operations = append(operations, server.NameServerGroupUpdateOperation{
				Type:   server.UpdateNameServerGroupName,
				Values: patch.Value,
			})
		case api.NameserverGroupPatchOperationPathDescription:
			operations = append(operations, server.NameServerGroupUpdateOperation{
				Type:   server.UpdateNameServerGroupDescription,
				Values: patch.Value,
			})
		case api.NameserverGroupPatchOperationPathNameservers:
			operations = append(operations, server.NameServerGroupUpdateOperation{
				Type:   server.UpdateNameServerGroupNameServers,
				Values: patch.Value,
			})
		case api.NameserverGroupPatchOperationPathGroups:
			operations = append(operations, server.NameServerGroupUpdateOperation{
				Type:   server.UpdateNameServerGroupGroups,
				Values: patch.Value,
			})
		case api.NameserverGroupPatchOperationPathEnabled:
			operations = append(operations, server.NameServerGroupUpdateOperation{
				Type:   server.UpdateNameServerGroupEnabled,
				Values: patch.Value,
			})
		default:
			http.Error(w, "invalid patch path", http.StatusBadRequest)
			return
		}
	}

	updatedNSGroup, err := h.accountManager.UpdateNameServerGroup(account.Id, nsGroupID, operations)
	if err != nil {
		toHTTPError(err, w)
		return
	}

	resp := toNameserverGroupResponse(updatedNSGroup)

	writeJSONObject(w, &resp)
}

// DeleteNameserverGroupHandler handles nameserver group deletion request
func (h *Nameservers) DeleteNameserverGroupHandler(w http.ResponseWriter, r *http.Request) {
	account, err := getJWTAccount(h.accountManager, h.jwtExtractor, h.authAudience, r)
	if err != nil {
		log.Error(err)
		http.Redirect(w, r, "/", http.StatusInternalServerError)
		return
	}

	nsGroupID := mux.Vars(r)["id"]
	if len(nsGroupID) == 0 {
		http.Error(w, "invalid nameserver group ID", http.StatusBadRequest)
		return
	}

	err = h.accountManager.DeleteNameServerGroup(account.Id, nsGroupID)
	if err != nil {
		toHTTPError(err, w)
		return
	}

	writeJSONObject(w, "")
}

// GetNameserverGroupHandler handles a nameserver group Get request identified by ID
func (h *Nameservers) GetNameserverGroupHandler(w http.ResponseWriter, r *http.Request) {
	account, err := getJWTAccount(h.accountManager, h.jwtExtractor, h.authAudience, r)
	if err != nil {
		log.Error(err)
		http.Redirect(w, r, "/", http.StatusInternalServerError)
		return
	}

	nsGroupID := mux.Vars(r)["id"]
	if len(nsGroupID) == 0 {
		http.Error(w, "invalid nameserver group ID", http.StatusBadRequest)
		return
	}

	nsGroup, err := h.accountManager.GetNameServerGroup(account.Id, nsGroupID)
	if err != nil {
		toHTTPError(err, w)
		return
	}

	resp := toNameserverGroupResponse(nsGroup)

	writeJSONObject(w, &resp)

}

func toServerNSList(apiNSList []api.Nameserver) ([]nbdns.NameServer, error) {
	var nsList []nbdns.NameServer
	for _, apiNS := range apiNSList {
		parsed, err := nbdns.ParseNameServerURL(fmt.Sprintf("%s://%s:%d", apiNS.NsType, apiNS.Ip, apiNS.Port))
		if err != nil {
			return nil, err
		}
		nsList = append(nsList, parsed)
	}

	return nsList, nil
}

func toNameserverGroupResponse(serverNSGroup *nbdns.NameServerGroup) *api.NameserverGroup {
	var nsList []api.Nameserver
	for _, ns := range serverNSGroup.NameServers {
		apiNS := api.Nameserver{
			Ip:     ns.IP.String(),
			NsType: api.NameserverNsType(ns.NSType.String()),
			Port:   ns.Port,
		}
		nsList = append(nsList, apiNS)
	}

	return &api.NameserverGroup{
		Id:          serverNSGroup.ID,
		Name:        serverNSGroup.Name,
		Description: serverNSGroup.Description,
		Groups:      serverNSGroup.Groups,
		Nameservers: nsList,
		Enabled:     serverNSGroup.Enabled,
	}
}
