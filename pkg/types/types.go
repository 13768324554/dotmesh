package types

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/dotmesh-io/dotmesh/pkg/user"
)

type CloneWithName struct {
	Name  string
	Clone Clone
}
type ClonesList []CloneWithName

type dirtyInfo struct {
	Server     string
	DirtyBytes int64
	SizeBytes  int64
}

type PermissionDenied struct {
}

func (e PermissionDenied) Error() string {
	return "Permission denied."
}

type VolumesAndBranches struct {
	Dots    []TopLevelFilesystem
	Servers []Server
}

type Server struct {
	Id        string
	Addresses []string
}

type ByAddress []Server

type dotmeshVolumeByName []DotmeshVolume

func (v dotmeshVolumeByName) Len() int {
	return len(v)
}

func (v dotmeshVolumeByName) Swap(i, j int) {
	v[i], v[j] = v[j], v[i]
}

func (v dotmeshVolumeByName) Less(i, j int) bool {
	return v[i].Name.Name < v[j].Name.Name
}

type DotmeshVolumeAndContainers struct {
	Volume     DotmeshVolume
	Containers []DockerContainer
}

type DockerContainer struct {
	Name string
	Id   string
}

type VersionInfo struct {
	InstalledVersion    string `json:"installed_version"`
	CurrentVersion      string `json:"current_version"`
	CurrentReleaseDate  int    `json:"current_release_date"`
	CurrentDownloadURL  string `json:"current_download_url"`
	CurrentChangelogURL string `json:"current_changelog_url"`
	ProjectWebsite      string `json:"project_website"`
	Outdated            bool   `json:"outdated"`
}

type SafeConfig struct {
}

type Origin struct {
	FilesystemId string
	SnapshotId   string
}

type metadata map[string]string
type snapshot struct {
	// exported for json serialization
	Id       string
	Metadata *metadata
	// private (do not serialize)
	filesystem *filesystem
}

type Clone struct {
	FilesystemId string
	Origin       Origin
}

type filesystem struct {
	id        string
	exists    bool
	mounted   bool
	snapshots []*snapshot
	// support filesystem which is clone of another filesystem, for branching
	// purposes, with origin e.g. "<fs-uuid-of-actual-origin-snapshot>@<snap-id>"
	origin Origin
}

type S3TransferRequest struct {
	KeyID           string
	SecretKey       string
	Prefixes        []string
	Endpoint        string
	Direction       string
	LocalNamespace  string
	LocalName       string
	LocalBranchName string
	RemoteName      string
}

func (transferRequest S3TransferRequest) String() string {
	v := reflect.ValueOf(transferRequest)
	toString := ""
	for i := 0; i < v.NumField(); i++ {
		fieldName := v.Type().Field(i).Name
		if fieldName == "SecretKey" {
			toString = toString + fmt.Sprintf(" %v=%v,", fieldName, "****")
		} else {
			toString = toString + fmt.Sprintf(" %v=%v,", fieldName, v.Field(i).Interface())
		}
	}
	return toString
}

type TransferRequest struct {
	Peer             string // hostname
	User             string
	Port             int
	ApiKey           string //protected value in toString
	Direction        string // "push" or "pull"
	LocalNamespace   string
	LocalName        string
	LocalBranchName  string
	RemoteNamespace  string
	RemoteName       string
	RemoteBranchName string
	// TODO could also include SourceSnapshot here
	TargetCommit string // optional, "" means "latest"
}

func (transferRequest TransferRequest) String() string {
	v := reflect.ValueOf(transferRequest)
	toString := ""
	for i := 0; i < v.NumField(); i++ {
		fieldName := v.Type().Field(i).Name
		if fieldName == "ApiKey" {
			toString = toString + fmt.Sprintf(" %v=%v,", fieldName, "****")
		} else {
			toString = toString + fmt.Sprintf(" %v=%v,", fieldName, v.Field(i).Interface())
		}
	}
	return toString
}

type EventArgs map[string]interface{}
type Event struct {
	Name string
	Args *EventArgs
}

func (ea EventArgs) String() string {
	aggr := []string{}
	for k, v := range ea {
		aggr = append(aggr, fmt.Sprintf("%s: %+q", k, v))
	}
	return strings.Join(aggr, ", ")
}

func (e Event) String() string {
	return fmt.Sprintf("<Event %s: %s>", e.Name, e.Args)
}

type TransferPollResult struct {
	TransferRequestId string
	Peer              string // hostname
	User              string
	ApiKey            string
	Direction         string // "push" or "pull"

	// Hold onto this information, it might become useful for e.g. recursive
	// receives of clone filesystems.
	LocalNamespace   string
	LocalName        string
	LocalBranchName  string
	RemoteNamespace  string
	RemoteName       string
	RemoteBranchName string

	// Same across both clusters
	FilesystemId string

	// TODO add clusterIds? probably comes from etcd. in fact, could be the
	// discovery id (although that is only for bootstrap... hmmm).
	InitiatorNodeId string
	PeerNodeId      string

	// XXX a Transfer that spans multiple filesystem ids won't have a unique
	// starting/target snapshot, so this is in the wrong place right now.
	// although maybe it makes sense to talk about a target *final* snapshot,
	// with interim snapshots being an implementation detail.
	StartingCommit string
	TargetCommit   string

	Index              int    // i.e. transfer 1/4 (Index=1)
	Total              int    //                   (Total=4)
	Status             string // one of "starting", "running", "finished", "error"
	NanosecondsElapsed int64
	Size               int64 // size of current segment in bytes
	Sent               int64 // number of bytes of current segment sent so far
	Message            string
}

type Config struct {
	FilesystemMetadataTimeout int64
	UserManager               user.UserManager
}

type PathToTopLevelFilesystem struct {
	TopLevelFilesystemId   string
	TopLevelFilesystemName VolumeName
	Clones                 ClonesList
}

// refers to a clone's "pointer" to a filesystem id and its snapshot.
//
// note that a clone's Origin's FilesystemId may differ from the "top level"
// filesystemId in the Registry's Clones map if the clone is attributed to a
// top-level filesystem which is *transitively* its parent but not its direct
// parent. In this case the Origin FilesystemId will always point to its direct
// parent.

func castToMetadata(val interface{}) metadata {
	meta, ok := val.(metadata)
	if !ok {
		meta = metadata{}
		// massage the data into the right type
		cast := val.(map[string]interface{})
		for k, v := range cast {
			meta[k] = v.(string)
		}
	}
	return meta
}

type Prelude struct {
	SnapshotProperties []*snapshot
}

type containerInfo struct {
	Server     string
	Containers []DockerContainer
}