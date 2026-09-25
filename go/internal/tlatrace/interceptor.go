package tlatrace

import (
	"context"
	"errors"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"
)

// Interceptor records each call as a new Request: a Receive of the command
// on its stream when it arrives, and an Answer with what the caller was told
// when it returns. The handler's context carries the Request, so the Store
// calls it makes in between are attributed to it.
func Interceptor(rec *Recorder) twirp.Interceptor {
	return func(next twirp.Method) twirp.Method {
		return func(ctx context.Context, request any) (any, error) {
			req := rec.nextRequest()
			command, _ := twirp.MethodName(ctx)
			var stream string
			if r, ok := request.(interface{ GetId() string }); ok {
				stream = r.GetId()
			}
			rec.record(Record{Kind: KindReceive, Req: req, Command: command, Stream: stream})

			resp, err := next(WithRequest(ctx, req), request)

			rec.record(Record{Kind: KindAnswer, Req: req, Stream: stream, Outcome: answer(resp, err)})
			return resp, err
		}
	}
}

// answer is what the caller was told: the event its command was decided as,
// or, on an error, Aborted or Failed, which tell it nothing either way.
func answer(resp any, err error) string {
	if err != nil {
		var terr twirp.Error
		if errors.As(err, &terr) && terr.Code() == twirp.Aborted {
			return OutcomeAborted
		}
		return OutcomeFailed
	}
	if msg, ok := resp.(proto.Message); ok {
		if event, decided := decision(msg); decided {
			return event
		}
	}
	return OutcomeUndecided
}
