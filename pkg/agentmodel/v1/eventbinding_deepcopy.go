package v1

// Hand-written deepcopy for the EventBinding spec/status types (kept out of the
// generated zz_generated.deepcopy.go, which is not blindly regenerated). The
// types are flat structs of scalars plus one *metav1.Time pointer.

// DeepCopyInto copies the receiver into out.
func (in *EventFilter) DeepCopyInto(out *EventFilter) { *out = *in }

// DeepCopy returns a deep copy of the receiver.
func (in *EventFilter) DeepCopy() *EventFilter {
	if in == nil {
		return nil
	}
	out := new(EventFilter)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *EventTarget) DeepCopyInto(out *EventTarget) { *out = *in }

// DeepCopy returns a deep copy of the receiver.
func (in *EventTarget) DeepCopy() *EventTarget {
	if in == nil {
		return nil
	}
	out := new(EventTarget)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *EventBindingSpec) DeepCopyInto(out *EventBindingSpec) {
	*out = *in
	out.Filter = in.Filter
	out.Target = in.Target
}

// DeepCopy returns a deep copy of the receiver.
func (in *EventBindingSpec) DeepCopy() *EventBindingSpec {
	if in == nil {
		return nil
	}
	out := new(EventBindingSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out (deep-copies the LastEventTime ptr).
func (in *EventBindingStatus) DeepCopyInto(out *EventBindingStatus) {
	*out = *in
	if in.LastEventTime != nil {
		out.LastEventTime = in.LastEventTime.DeepCopy()
	}
}

// DeepCopy returns a deep copy of the receiver.
func (in *EventBindingStatus) DeepCopy() *EventBindingStatus {
	if in == nil {
		return nil
	}
	out := new(EventBindingStatus)
	in.DeepCopyInto(out)
	return out
}
