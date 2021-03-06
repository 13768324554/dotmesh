package registry

import (
	"context"
	"encoding/json"
	"fmt"
	// "log"
	"sort"
	"sync"

	"github.com/coreos/etcd/client"

	"github.com/dotmesh-io/dotmesh/pkg/auth"
	"github.com/dotmesh-io/dotmesh/pkg/types"
	"github.com/dotmesh-io/dotmesh/pkg/user"

	log "github.com/sirupsen/logrus"
)

// A branch is just another filesystem, but which exists as a ZFS clone of a
// snapshot of another (filesystem or clone).
//
// The Registry allows us to record both top level filesystem name => id
// mappings, as well as knowledge about clones and their origins (the
// filesystem id and snapshot from which they were cloned).

const DEFAULT_BRANCH = "master"

type Registry interface {
	Filesystems() []types.VolumeName
	IdFromName(name types.VolumeName) (string, error)
	GetByName(name types.VolumeName) (types.TopLevelFilesystem, error)
	// FilesystemIds() []string
	FilesystemIdsIncludingClones() []string
	DeducePathToTopLevelFilesystem(name types.VolumeName, cloneName string) (types.PathToTopLevelFilesystem, error)

	ClonesFor(filesystemID string) map[string]types.Clone

	RegisterFilesystem(ctx context.Context, name types.VolumeName, filesystemID string) error
	UnregisterFilesystem(name types.VolumeName) error

	UpdateCollaborators(ctx context.Context, tlf types.TopLevelFilesystem, newCollaborators []user.SafeUser) error
	RegisterClone(name string, topLevelFilesystemId string, clone types.Clone) error

	// TODO: why ..FromEtcd?
	UpdateFilesystemFromEtcd(name types.VolumeName, rf types.RegistryFilesystem) error
	UpdateCloneFromEtcd(name string, topLevelFilesystemId string, clone types.Clone)
	DeleteCloneFromEtcd(name string, topLevelFilesystemId string)

	LookupFilesystem(name types.VolumeName) (types.TopLevelFilesystem, error)
	LookupClone(topLevelFilesystemId, cloneName string) (types.Clone, error)
	LookupCloneById(filesystemId string) (types.Clone, error)
	LookupCloneByIdWithName(filesystemId string) (types.Clone, string, error)
	LookupFilesystemById(filesystemId string) (types.TopLevelFilesystem, string, error)

	Exists(name types.VolumeName, cloneName string) string

	MaybeCloneFilesystemId(name types.VolumeName, cloneName string) (string, error)

	DumpTopLevelFilesystems() []*types.TopLevelFilesystem
	DumpClones() map[string]map[string]types.Clone
}

type DefaultRegistry struct {
	// filesystems ~= repos, top-level filesystems
	// map user facing filesystem name => filesystemId, with implicit null
	// origin
	topLevelFilesystems     map[types.VolumeName]types.TopLevelFilesystem
	topLevelFilesystemsLock *sync.RWMutex
	// clones ~= branches
	// map filesystem.id (of topLevelFilesystem the clone is attributed to - ie
	// not another clone) => user facing *branch name* => filesystemId,origin pair
	clones     map[string]map[string]types.Clone
	clonesLock *sync.RWMutex

	userManager user.UserManager

	etcdClient client.KeysAPI
	prefix     string
}

func NewRegistry(um user.UserManager, etcdClient client.KeysAPI, prefix string) *DefaultRegistry {
	return &DefaultRegistry{
		topLevelFilesystems:     map[types.VolumeName]types.TopLevelFilesystem{},
		clones:                  map[string]map[string]types.Clone{},
		topLevelFilesystemsLock: &sync.RWMutex{},
		clonesLock:              &sync.RWMutex{},
		userManager:             um,
		etcdClient:              etcdClient,
		prefix:                  prefix,
	}
}

func (r *DefaultRegistry) DeducePathToTopLevelFilesystem(name types.VolumeName, cloneName string) (types.PathToTopLevelFilesystem, error) {
	/*
		Need to give the peer enough information to recreate an entire path from
		root to leaf of clone metadata. Example:

			master
			|- branch1
			\- branch2
		       \- branch2b

		If this filesystem id represents branch2b, the response would be
		[]string{"master", "branch2", "branch2b"}

		Except, it actually needs to be []Clone{...} with each clone referring
		to its origin, so that the appropriate data can be reproduced in the
		peer's registry.

	*/
	log.Printf("[DeducePathToTopLevelFilesystem] looking up %s", name)
	tlf, err := r.LookupFilesystem(name)
	if err != nil {
		log.Printf(
			"[DeducePathToTopLevelFilesystem] error looking up %s: %s",
			name, err,
		)
		return types.PathToTopLevelFilesystem{}, err
	}
	log.Printf(
		"[DeducePathToTopLevelFilesystem] looking up maybe-clone pair %s,%s",
		name, cloneName,
	)
	filesystemId, err := r.MaybeCloneFilesystemId(name, cloneName)
	if err != nil {
		log.Printf(
			"[DeducePathToTopLevelFilesystem] error looking up maybe-clone %s,%s: %s",
			name, cloneName, err,
		)
		return types.PathToTopLevelFilesystem{}, err
	}
	nextFilesystemId := filesystemId

	clist := types.ClonesList{}

	for {
		log.Printf(
			"[DeducePathToTopLevelFilesystem] %s == %s ?",
			nextFilesystemId, tlf.MasterBranch.Id,
		)
		// base case - nextFilesystemId is the top level one.
		if nextFilesystemId == tlf.MasterBranch.Id {
			return types.PathToTopLevelFilesystem{
				TopLevelFilesystemId:   nextFilesystemId,
				TopLevelFilesystemName: name,
				Clones:                 clist, // empty on first iteration
			}, nil
		}
		// inductive step - resolve nextFilesystemId into its clone, if it is a
		// clone. if it's not a clone, and it's not a top level filesystem,
		// throw an error.
		clone, cloneName, err := r.LookupCloneByIdWithName(nextFilesystemId)
		if err != nil {
			return types.PathToTopLevelFilesystem{}, err
		}
		// append to beginning of list, because they need to be created in the
		// reverse order of traversal. (traversal is from tip to root, we want
		// to return the list from the root to tip.)
		clist = append(types.ClonesList{types.CloneWithName{Name: cloneName, Clone: clone}}, clist...)
		nextFilesystemId = clone.Origin.FilesystemId
	}
}

type ByNames []types.VolumeName

func (bn ByNames) Len() int      { return len(bn) }
func (bn ByNames) Swap(i, j int) { bn[i], bn[j] = bn[j], bn[i] }
func (bn ByNames) Less(i, j int) bool {
	return bn[i].Namespace < bn[j].Namespace ||
		bn[i].Name < bn[j].Name
}

// sorted list of top-level filesystem names
func (r *DefaultRegistry) Filesystems() []types.VolumeName {
	r.topLevelFilesystemsLock.RLock()
	defer r.topLevelFilesystemsLock.RUnlock()
	filesystemNames := []types.VolumeName{}
	for name, _ := range r.topLevelFilesystems {
		filesystemNames = append(filesystemNames, name)
	}
	sort.Sort(ByNames(filesystemNames))
	return filesystemNames
}

func (r *DefaultRegistry) IdFromName(name types.VolumeName) (string, error) {
	tlf, err := r.GetByName(name)
	if err != nil {
		return "", err
	}
	return tlf.MasterBranch.Id, nil
}

func (r *DefaultRegistry) GetByName(name types.VolumeName) (types.TopLevelFilesystem, error) {
	r.topLevelFilesystemsLock.RLock()
	defer r.topLevelFilesystemsLock.RUnlock()
	tlf, ok := r.topLevelFilesystems[name]
	if !ok {
		return types.TopLevelFilesystem{},
			fmt.Errorf("No such top-level filesystem")
	}
	return tlf, nil
}

// // list of top-level filesystem ids
// func (r *DefaultRegistry) FilesystemIds() []string {
// 	r.topLevelFilesystemsLock.RLock()
// 	defer r.topLevelFilesystemsLock.RUnlock()
// 	filesystemIds := []string{}
// 	for _, tlf := range r.topLevelFilesystems {
// 		filesystemIds = append(filesystemIds, tlf.MasterBranch.Id)
// 	}
// 	sort.Strings(filesystemIds)
// 	return filesystemIds
// }

func (r *DefaultRegistry) FilesystemIdsIncludingClones() []string {
	filesystemIds := []string{}

	r.topLevelFilesystemsLock.RLock()
	for _, tlf := range r.topLevelFilesystems {
		filesystemIds = append(filesystemIds, tlf.MasterBranch.Id)
	}
	r.topLevelFilesystemsLock.RUnlock()

	r.clonesLock.RLock()
	for _, clones := range r.clones {
		for _, clone := range clones {
			filesystemIds = append(filesystemIds, clone.FilesystemId)
		}
	}
	r.clonesLock.RUnlock()

	sort.Strings(filesystemIds)
	return filesystemIds
}

// map of clone names => clone objects for a given top-level filesystemId
func (r *DefaultRegistry) ClonesFor(filesystemId string) map[string]types.Clone {
	r.clonesLock.RLock()
	defer r.clonesLock.RUnlock()
	_, ok := r.clones[filesystemId]
	if !ok {
		// filesystemId not found, return empty map
		return map[string]types.Clone{}
	}
	return r.clones[filesystemId]
}

// Check whether a given clone can be pulled onto this machine, based on
// whether its origin snapshot exists here
// func (r *DefaultRegistry) CanPullClone(c Clone) bool {
// 	r.state.filesystemsLock.RLock()
// 	fsMachine, ok := r.state.filesystems[c.Origin.FilesystemId]
// 	r.state.filesystemsLock.RUnlock()
// 	if !ok {
// 		return false
// 	}
// 	fsMachine.snapshotsLock.Lock()
// 	defer fsMachine.snapshotsLock.Lock()
// 	for _, snap := range fsMachine.filesystem.snapshots {
// 		if snap.Id == c.Origin.SnapshotId {
// 			return true
// 		}
// 	}
// 	return false
// }

// the type as stored in the json in etcd (intermediate representation wrt
// DotmeshVolume)
type registryFilesystem struct {
	Id              string
	OwnerId         string
	CollaboratorIds []string
}

// update a filesystem, including updating etcd and our local state
func (r *DefaultRegistry) RegisterFilesystem(ctx context.Context, name types.VolumeName, filesystemId string) error {
	authenticatedUserId := auth.GetUserIDFromCtx(ctx)
	if authenticatedUserId == "" {
		return fmt.Errorf("No user found in request context.")
	}
	rf := types.RegistryFilesystem{
		Id: filesystemId,
		// Owner is, for now, always the authenticated user at the time of
		// creation
		OwnerId: authenticatedUserId,
	}
	serialized, err := json.Marshal(rf)
	if err != nil {
		return err
	}
	_, err = r.etcdClient.Set(
		context.Background(),
		// (0)/(1)dotmesh.io/(2)registry/(3)filesystems/(4)<namespace>/(5)<name> =>
		//     {"Uuid": "<fs-uuid>"}
		fmt.Sprintf("%s/registry/filesystems/%s/%s", r.prefix, name.Namespace, name.Name),
		string(serialized),
		// we support updates in UpdateCollaborators, below.
		&client.SetOptions{PrevExist: client.PrevNoExist},
	)
	if err != nil {
		return err
	}
	// Only update our local belief system once the write to etcd has been
	// successful!
	return r.UpdateFilesystemFromEtcd(name, rf)
}

// Remove a filesystem from the registry
func (r *DefaultRegistry) UnregisterFilesystem(name types.VolumeName) error {
	_, err := r.etcdClient.Delete(
		context.Background(),
		// (0)/(1)dotmesh.io/(2)registry/(3)filesystems/(4)<namespace>/(5)<name> =>
		//     {"Uuid": "<fs-uuid>"}
		fmt.Sprintf("%s/registry/filesystems/%s/%s", r.prefix, name.Namespace, name.Name),
		&client.DeleteOptions{},
	)
	return err
}

func (r *DefaultRegistry) UpdateCollaborators(ctx context.Context, tlf types.TopLevelFilesystem, newCollaborators []user.SafeUser) error {

	collaboratorIds := []string{}
	for _, u := range newCollaborators {
		collaboratorIds = append(collaboratorIds, u.Id)
	}
	rf := types.RegistryFilesystem{
		Id: tlf.MasterBranch.Id,
		// Owner is, for now, always the authenticated user at the time of
		// creation
		OwnerId:         tlf.Owner.Id,
		CollaboratorIds: collaboratorIds,
	}
	serialized, err := json.Marshal(rf)
	if err != nil {
		return err
	}

	_, err = r.etcdClient.Set(
		context.Background(),
		// (0)/(1)dotmesh.io/(2)registry/(3)filesystems/(4)<namespace>/(5)<name> =>
		//     {"Uuid": "<fs-uuid>"}
		fmt.Sprintf("%s/registry/filesystems/%s/%s", r.prefix, tlf.MasterBranch.Name.Namespace, tlf.MasterBranch.Name.Name),
		string(serialized),
		// allow (and require) update over existing.
		&client.SetOptions{PrevExist: client.PrevExist},
	)
	if err != nil {
		log.WithFields(log.Fields{
			"path:": fmt.Sprintf("%s/registry/filesystems/%s/%s", r.prefix, tlf.MasterBranch.Name.Namespace, tlf.MasterBranch.Name.Name),
			"error": err,
		}).Error("failed to update registry filesystems")
		return err
	}
	// Only update our local belief system once the write to etcd has been
	// successful!

	log.Infof("updating after adding collab: %v", newCollaborators)
	return r.UpdateFilesystemFromEtcd(tlf.MasterBranch.Name, rf)
}

// update a clone, including updating our local record and etcd
func (r *DefaultRegistry) RegisterClone(name string, topLevelFilesystemId string, clone types.Clone) error {
	r.UpdateCloneFromEtcd(name, topLevelFilesystemId, clone)
	// kapi, err := getEtcdKeysApi()
	// if err != nil {
	// 	return err
	// }
	serialized, err := json.Marshal(clone)
	if err != nil {
		return err
	}
	_, err = r.etcdClient.Set(
		context.Background(),
		// (0)/(1)dotmesh.io/(2)registry/(3)clones/(4)<fs-uuid-of-filesystem>/(5)<name> =>
		//     {"Origin": {"FilesystemId": "<fs-uuid-of-actual-origin-snapshot>", "SnapshotId": "<snap-id>"}, "Uuid": "<fs-uuid>"}
		fmt.Sprintf("%s/registry/clones/%s/%s", r.prefix, topLevelFilesystemId, name),
		string(serialized),
		&client.SetOptions{PrevExist: client.PrevNoExist},
	)

	return err
}

// func safeUser(u User) SafeUser {
// 	h := md5.New()
// 	io.WriteString(h, u.Email)
// 	emailHash := fmt.Sprintf("%x", h.Sum(nil))
// 	return SafeUser{
// 		Id:        u.Id,
// 		Name:      u.Name,
// 		Email:     u.Email,
// 		EmailHash: emailHash,
// 		Metadata:  u.Metadata,
// 	}
// }

func (r *DefaultRegistry) UpdateFilesystemFromEtcd(name types.VolumeName, rf types.RegistryFilesystem) error {
	r.topLevelFilesystemsLock.Lock()
	defer r.topLevelFilesystemsLock.Unlock()

	if rf.Id == "" {
		// Deletion
		log.Printf("[UpdateFilesystemFromEtcd] %s => GONE", name)
		delete(r.topLevelFilesystems, name)
	} else {

		// Creation or Update
		// us, err := AllUsers()
		// if err != nil {
		// 	return err
		// }
		// umap := map[string]User{}
		// for _, u := range us {
		// 	umap[u.Id] = u
		// }

		// owner, ok := umap[rf.OwnerId]
		owner, err := r.userManager.Get(&user.Query{
			Ref: rf.OwnerId,
		})
		if err != nil {
			return fmt.Errorf("Unable to locate owner %v.", rf.OwnerId)
		}

		collaborators := []user.SafeUser{}
		for _, c := range rf.CollaboratorIds {
			cUser, err := r.userManager.Get(&user.Query{Ref: c})
			if err != nil {
				return fmt.Errorf("Unable to locate collaborator: %s", err)
			}
			collaborators = append(collaborators, cUser.SafeUser())
		}

		log.Printf("[UpdateFilesystemFromEtcd] %s => %s", name, rf.Id)
		r.topLevelFilesystems[name] = types.TopLevelFilesystem{
			// XXX: Hmm, I wonder if it's OK to just put minimal information here.
			// Probably not! We should construct a real TopLevelFilesystem object
			// if that's even the right level of abstraction. At time of writing,
			// the only thing that seems to reasonably construct a
			// TopLevelFilesystem is rpc's AllVolumesAndClones.
			MasterBranch:  types.DotmeshVolume{Id: rf.Id, Name: name},
			Owner:         owner.SafeUser(),
			Collaborators: collaborators,
		}
	}
	return nil
}

func (r *DefaultRegistry) UpdateCloneFromEtcd(name string, topLevelFilesystemId string, clone types.Clone) {
	r.clonesLock.Lock()
	defer r.clonesLock.Unlock()

	if _, ok := r.clones[topLevelFilesystemId]; !ok {
		r.clones[topLevelFilesystemId] = map[string]types.Clone{}
	}
	r.clones[topLevelFilesystemId][name] = clone
}

func (r *DefaultRegistry) DeleteCloneFromEtcd(name string, topLevelFilesystemId string) {
	r.clonesLock.Lock()
	defer r.clonesLock.Unlock()

	delete(r.clones, topLevelFilesystemId)
}

func (r *DefaultRegistry) LookupFilesystem(name types.VolumeName) (types.TopLevelFilesystem, error) {
	r.topLevelFilesystemsLock.RLock()
	defer r.topLevelFilesystemsLock.RUnlock()
	if _, ok := r.topLevelFilesystems[name]; !ok {
		return types.TopLevelFilesystem{}, fmt.Errorf("No such filesystem named '%s'", name)
	}
	return r.topLevelFilesystems[name], nil
}

// Look up a clone. If you want to look up based on filesystem name and clone name, do:
// fsId := LookupFilesystem(fsName); cloneId := LookupClone(fsId, cloneName)
func (r *DefaultRegistry) LookupClone(topLevelFilesystemId, cloneName string) (types.Clone, error) {
	r.clonesLock.RLock()
	defer r.clonesLock.RUnlock()
	if _, ok := r.clones[topLevelFilesystemId]; !ok {
		return types.Clone{}, fmt.Errorf("No clones at all, let alone named '%s' for filesystem id '%s'", cloneName, topLevelFilesystemId)
	}
	if _, ok := r.clones[topLevelFilesystemId][cloneName]; !ok {
		return types.Clone{}, fmt.Errorf("No clone named '%s' for filesystem id '%s'", cloneName, topLevelFilesystemId)
	}
	return r.clones[topLevelFilesystemId][cloneName], nil
}

// XXX make this more efficient
func (r *DefaultRegistry) LookupCloneById(filesystemId string) (types.Clone, error) {
	c, _, err := r.LookupCloneByIdWithName(filesystemId)
	return c, err
}

func (r *DefaultRegistry) LookupCloneByIdWithName(filesystemId string) (types.Clone, string, error) {
	r.clonesLock.RLock()
	defer r.clonesLock.RUnlock()
	for _, cloneMap := range r.clones {
		for cloneName, clone := range cloneMap {
			if clone.FilesystemId == filesystemId {
				return clone, cloneName, nil
			}
		}
	}
	return types.Clone{}, "", NoSuchClone{filesystemId}
}

// given a filesystem id, return the (types.TopLevelFilesystem, cloneName) tuple that it
// can be identified by to the user.
// XXX make this less horrifically inefficient by storing & updating inverted
// indexes.
func (r *DefaultRegistry) LookupFilesystemById(filesystemId string) (types.TopLevelFilesystem, string, error) {
	r.topLevelFilesystemsLock.RLock()
	defer r.topLevelFilesystemsLock.RUnlock()
	r.clonesLock.RLock()
	defer r.clonesLock.RUnlock()
	for _, tlf := range r.topLevelFilesystems {
		if tlf.MasterBranch.Id == filesystemId {
			// empty-string cloneName ~= "master branch"
			log.Debugf("[LookupFilesystemById] result: %+v, clone: master", tlf)
			return tlf, "", nil
		}
	}
	for topLevelFilesystemId, cloneMap := range r.clones {
		for cloneName, clone := range cloneMap {
			if clone.FilesystemId == filesystemId {
				// find the tlf for this topLevelFilesystemId
				for _, tlf := range r.topLevelFilesystems {
					if tlf.MasterBranch.Id == topLevelFilesystemId {
						log.Debugf("[LookupFilesystemById] result: %+v, clone: %v", tlf, cloneName)

						return tlf, cloneName, nil
					}
				}
			}
		}
	}

	return types.TopLevelFilesystem{}, "", fmt.Errorf(
		"Unable to find user-facing filesystemName, cloneName for filesystem id %s",
		filesystemId,
	)
}

// filesystem id if exists, else ""
func (r *DefaultRegistry) Exists(name types.VolumeName, cloneName string) string {
	r.topLevelFilesystemsLock.RLock()
	defer r.topLevelFilesystemsLock.RUnlock()
	tlf, ok := r.topLevelFilesystems[name]
	if !ok {
		return ""
	}
	filesystemId := tlf.MasterBranch.Id
	if cloneName != "" {
		r.clonesLock.RLock()
		defer r.clonesLock.RUnlock()
		if _, ok := r.clones[filesystemId]; !ok {
			return ""
		}
		clone, ok := r.clones[filesystemId][cloneName]
		if !ok {
			return ""
		}
		filesystemId = clone.FilesystemId
	}
	return filesystemId
}

// given a top level fs name and a clone name, find the appropriate fs id
func (r *DefaultRegistry) MaybeCloneFilesystemId(name types.VolumeName, cloneName string) (string, error) {
	tlf, err := r.LookupFilesystem(
		name,
	)
	if err != nil {
		return "", err
	}
	tlfId := tlf.MasterBranch.Id
	if cloneName != "" {
		// potentially resolve a clone's filesystem id, clobbering filesystemId
		clone, err := r.LookupClone(tlfId, cloneName)
		if err != nil {
			return "", err
		}
		tlfId = clone.FilesystemId
	}
	return tlfId, nil
}

func (r *DefaultRegistry) DumpTopLevelFilesystems() []*types.TopLevelFilesystem {
	r.topLevelFilesystemsLock.RLock()
	defer r.topLevelFilesystemsLock.RUnlock()
	result := []*types.TopLevelFilesystem{}
	for _, tlf := range r.topLevelFilesystems {
		tlfCopy := new(types.TopLevelFilesystem)
		*tlfCopy = tlf
		copy(tlfCopy.OtherBranches, tlf.OtherBranches)
		copy(tlfCopy.Collaborators, tlf.Collaborators)
		result = append(result, tlfCopy)
	}
	return result
}

func (r *DefaultRegistry) DumpClones() map[string]map[string]types.Clone {
	r.clonesLock.RLock()
	defer r.clonesLock.RUnlock()
	result := make(map[string]map[string]types.Clone, len(r.clones))

	for id, v := range r.clones {

		if _, ok := result[id]; !ok {
			result[id] = make(map[string]types.Clone)
		}
		for k, c := range v {
			result[id][k] = c
		}
	}
	return r.clones
}
