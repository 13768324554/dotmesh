package statemachine

func backoffStateWithReason(reason string) func(f *fsMachine) stateFn {
	return func(f *fsMachine) stateFn {
		f.transitionedTo("backoff", fmt.Sprintf("pausing due to %s", reason))
		log.Printf("entering backoff state for %s", f.filesystemId)
		// TODO if we know that we're supposed to be mounted or unmounted, based on
		// etcd state, actually put us back into the required state rather than
		// just passively going back into discovering... or maybe, do that in
		// discoveringState?
		time.Sleep(time.Second)
		return discoveringState
	}
}

func backoffState(f *fsMachine) stateFn {
	f.transitionedTo("backoff", "pausing")
	log.Printf("entering backoff state for %s", f.filesystemId)
	// TODO if we know that we're supposed to be mounted or unmounted, based on
	// etcd state, actually put us back into the required state rather than
	// just passively going back into discovering... or maybe, do that in
	// discoveringState?
	time.Sleep(time.Second)
	return discoveringState
}
