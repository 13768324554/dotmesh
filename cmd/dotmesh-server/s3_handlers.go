package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	// "log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/dotmesh-io/dotmesh/pkg/auth"
	dmclient "github.com/dotmesh-io/dotmesh/pkg/client"
	"github.com/dotmesh-io/dotmesh/pkg/user"
	"github.com/gorilla/mux"

	log "github.com/sirupsen/logrus"
)

type S3Handler struct {
	state *InMemoryState
	httputil.ReverseProxy
}

func NewS3Handler(state *InMemoryState) http.Handler {
	h := &S3Handler{
		state: state,
	}

	h.ReverseProxy.Director = h.Director

	return h
}

func (s *S3Handler) Director(req *http.Request) {
	target, ok := ctxGetAddress(req.Context())
	if !ok || target == "" {
		log.WithFields(log.Fields{
			"host": req.Host,
		}).Error("no target")

		_, cancel := context.WithCancel(req.Context())
		cancel()

		return
	}

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	u, err := url.Parse(target)
	if err != nil {
		log.WithFields(log.Fields{
			"host":  req.Host,
			"error": err,
		}).Error("failed to parse URL")
		return
	}

	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host

	if _, ok := req.Header["User-Agent"]; !ok {
		// explicitly disable User-Agent so it's not set to default value
		req.Header.Set("User-Agent", "")
	}
}

type ctxKey string

const (
	ctxKeyTarget ctxKey = "target"
)

func ctxSetAddress(ctx context.Context, address string) context.Context {
	return context.WithValue(ctx, ctxKeyTarget, address)
}

func ctxGetAddress(ctx context.Context) (string, bool) {
	val := ctx.Value(ctxKeyTarget)
	if val == nil {
		return "", false
	}
	return val.(string), true
}

func (s *S3Handler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	volName := VolumeName{
		Name:      vars["name"],
		Namespace: vars["namespace"],
	}

	isAdmin, err := AuthenticatedUserIsNamespaceAdministrator(req.Context(), volName.Namespace)
	if err != nil {
		http.Error(resp, err.Error(), 401)
		return
	}
	if !isAdmin {
		http.Error(resp, "User is not the administrator of namespace "+volName.Namespace, 401)
		return
	}
	branch, ok := vars["branch"]
	bucketName := fmt.Sprintf("%s-%s", vars["namespace"], vars["name"])
	if !ok || branch == "master" {
		branch = ""
	} else {
		bucketName += "-" + branch
	}
	localFilesystemId := s.state.registry.Exists(
		volName, branch,
	)
	if localFilesystemId != "" {
		key, ok := vars["key"]
		if ok {
			switch req.Method {
			case "PUT":
				master := s.state.masterFor(localFilesystemId)
				if master != s.state.myNodeId {
					admin, err := s.state.userManager.Get(&user.Query{Ref: "admin"})
					if err != nil {
						http.Error(resp, fmt.Sprintf("Can't get API key to proxy s3 request: %+v.\n", err), 500)
						log.Errorf("can't get API key to proxy s3: %+v.", err)
						return
					}

					addresses := s.state.addressesFor(master)
					target, err := dmclient.DeduceUrl(context.Background(), addresses, "internal", "admin", admin.ApiKey) // FIXME, need master->name mapping, see how handover works normally
					if err != nil {
						http.Error(resp, err.Error(), 500)
						log.Errorf("can't establish URL to proxy s3: %+v.", err)
						return
					}
					log.Infof("[S3Handler.ServeHTTP] proxying PUT request to node: %s", target)
					s.ServeHTTP(resp, req.WithContext(ctxSetAddress(req.Context(), target)))
					return
				}

				s.putObject(resp, req, localFilesystemId, key)
			}

		} else {
			switch req.Method {
			case "GET":
				s.listBucket(resp, req, bucketName, localFilesystemId)
			}
		}

	} else {
		http.Error(resp, fmt.Sprintf("Bucket %s does not exist", bucketName), 404)
	}
}

func (s *S3Handler) putObject(resp http.ResponseWriter, req *http.Request, filesystemId, filename string) {
	user := auth.GetUserFromCtx(req.Context())
	fsm := s.state.initFilesystemMachine(filesystemId)

	if fsm.currentState != "active" {
		http.Error(resp, "please try again later", http.StatusServiceUnavailable)
		return
	}

	defer req.Body.Close()
	respCh := make(chan *Event)
	fsm.fileIO <- &File{
		Filename: filename,
		Contents: req.Body,
		User:     user.Name,
		Response: respCh,
	}

	result := <-respCh

	switch result.Name {
	case eventNameSaveFailed:
		e, ok := (*result.Args)["err"].(string)
		if ok {

			http.Error(resp, e, 500)
		}
		http.Error(resp, "upload failed", 500)
	default:
		resp.WriteHeader(200)
		resp.Header().Set("Access-Control-Allow-Origin", "*")
	}
}

type ListBucketResult struct {
	Name     string
	Prefix   string
	Contents []BucketObject
}

type BucketObject struct {
	Key          string
	LastModified time.Time
	Size         int64
}

func (s *S3Handler) listBucket(resp http.ResponseWriter, req *http.Request, name string, filesystemId string) {
	snapshots, err := s.state.snapshotsForCurrentMaster(filesystemId)
	if len(snapshots) == 0 {
		// throw up an error?
		http.Error(resp, "No snaps to mount - commit before listing.", 400)
		return
	}
	lastSnapshot := snapshots[len(snapshots)-1]
	responseChan, err := s.state.globalFsRequest(
		filesystemId,
		&Event{Name: "mount-snapshot",
			Args: &EventArgs{"snapId": lastSnapshot.Id}},
	)
	if err != nil {
		// error here
		log.Println(err)
		return
	}

	e := <-responseChan
	if e.Name == "mounted" {
		result := (*e.Args)["mount-path"].(string)
		keys, _, err := getKeysForDir(result+"/__default__", "")
		if err != nil {
			http.Error(resp, "failed to get keys for dir: "+err.Error(), 500)
			return
		}

		bucket := ListBucketResult{
			Name:     name,
			Contents: []BucketObject{},
		}
		for key, info := range keys {
			object := BucketObject{
				Key:          key,
				Size:         info.Size(),
				LastModified: info.ModTime(),
			}
			bucket.Contents = append(bucket.Contents, object)
		}
		response, err := xml.Marshal(bucket)
		if err != nil {
			http.Error(resp, fmt.Sprintf("failed to marshal response body: %s", err), 500)
			return
		}
		resp.WriteHeader(200)
		resp.Write(response)
	} else {

		log.Println(e)
		log.WithFields(log.Fields{
			"event":      e,
			"filesystem": filesystemId,
		}).Error("mount failed, returned event is not 'mounted'")
		http.Error(resp, fmt.Sprintf("failed to mount filesystem (%s), check logs", e.Name), 500)
	}
}
