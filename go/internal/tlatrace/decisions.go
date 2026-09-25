package tlatrace

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// resultOneof is where every command response in proto/ carries the event
// its command was decided as (e.g. RequestTransferResponse.result).
const resultOneof protoreflect.Name = "result"

// DecisionEvents lists the event types resp's command can be decided as: the
// messages its result oneof can hold. It is how a trace header learns the
// spec's Handlers from the proto instead of from a copy.
func DecisionEvents(resp proto.Message) []string {
	oneof := resp.ProtoReflect().Descriptor().Oneofs().ByName(resultOneof)
	if oneof == nil {
		return nil
	}
	fields := oneof.Fields()
	events := make([]string, 0, fields.Len())
	for i := range fields.Len() {
		if msg := fields.Get(i).Message(); msg != nil {
			events = append(events, string(msg.FullName()))
		}
	}
	return events
}

// decision returns the event type resp was decided as, or false when its
// result oneof is missing or empty.
func decision(resp proto.Message) (string, bool) {
	m := resp.ProtoReflect()
	oneof := m.Descriptor().Oneofs().ByName(resultOneof)
	if oneof == nil {
		return "", false
	}
	field := m.WhichOneof(oneof)
	if field == nil || field.Message() == nil {
		return "", false
	}
	return string(field.Message().FullName()), true
}
