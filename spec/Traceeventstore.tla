-------------------------- MODULE Traceeventstore --------------------------
(* Checks that a trace recorded by go/internal/tlatrace (see
   go/internal/transfer/tlatrace_test.go) is a behavior of eventstore: that
   the Go handlers took only steps the design allows.

   Every step here is one of eventstore's own actions, conjoined with what
   the trace line observed; nothing re-implements the design. Each line is
   consumed by exactly one step:

     Receive  Receive(r)
     Load     Load(r), with the stream as long as the database said
     Append   TryAppend(r) with the outcome the database gave, or Fail(r)
              when an error left nothing behind
     Answer   Fail(r) if r hasn't been answered yet, otherwise a check that
              the step that answered r told it what the caller was told

   plus one Submit(r) per Request, which consumes nothing. Lines are written
   after the database answers, so their order across Requests is not the
   order things happened in; only each Request's own lines are in order, and
   TLC searches for an interleaving that fits everything they observed. In
   Go every Request has its own goroutine, so each Request is its own
   Worker. *)
EXTENDS eventstore, Json, TLC, Sequences, Naturals, FiniteSets

CONSTANT TraceFile

Trace == ndJsonDeserialize(TraceFile)

TraceHeader == Trace[1]

Log == Tail(Trace)

----------------------------------------------------------------------------
(* eventstore's constants, as the trace and its header (taken from the Go
   code) give them *)

TraceRequests == {Log[i].req : i \in 1..Len(Log)}

(* the streams the Requests commanded, as their Receive lines name them *)
TraceIds == {Log[i].stream : i \in {j \in 1..Len(Log) : Log[j].kind = "Receive"}}

TraceCommands == DOMAIN TraceHeader.handlers

TraceHandlers == [c \in TraceCommands |->
                    {TraceHeader.handlers[c][i] : i \in 1..Len(TraceHeader.handlers[c])}]

TraceEvents == UNION {TraceHandlers[c] : c \in TraceCommands}

TraceMaxAttempts == TraceHeader.maxAttempts

TraceWorkers == TraceRequests

----------------------------------------------------------------------------

VARIABLE pos  \* how many of each Request's own lines have been matched

(* each Request's own lines, in the order it recorded them *)
Lines == [r \in TraceRequests |-> SelectSeq(Log, LAMBDA l: l.req = r)]

IsNext(r, kind) == pos[r] < Len(Lines[r]) /\ Lines[r][pos[r] + 1].kind = kind

Line(r) == Lines[r][pos[r] + 1]

Matched(r) == pos' = [pos EXCEPT ![r] = @ + 1]

(* a Load of an empty stream saw no types, and the line leaves them out *)
TypesOf(l) == IF "types" \in DOMAIN l THEN l.types ELSE << >>

TraceInit == Init /\ pos = [r \in TraceRequests |-> 0]

SubmitStep(r) == /\ pos[r] = 0
                 /\ Lines[r][1].kind = "Receive"
                 /\ Submit(r)
                 /\ request'[r] = <<Lines[r][1].stream, Lines[r][1].command>>
                 /\ UNCHANGED pos

ReceiveStep(r) == /\ IsNext(r, "Receive")
                  /\ Receive(r)
                  /\ handling'[r] = r
                  /\ Matched(r)

LoadStep(r) == /\ IsNext(r, "Load")
                /\ Line(r).stream = request[r][1]
                /\ Load(r)
                /\ Len(streams[Line(r).stream]) = Line(r).len
                /\ streams[Line(r).stream] = TypesOf(Line(r))
                /\ Matched(r)

AppendStep(r) == LET l  == Line(r)
                     id == request[r][1]
                 IN /\ IsNext(r, "Append")
                    /\ l.stream = id
                    /\ pc[r] = "append"
                    /\ l.expectedSeq = expectedSeq[r]
                    /\ l.types = <<decision[r]>>
                    /\ \/ /\ l.outcome \in {"ok", "error"}  \* it landed
                          /\ Len(streams[id]) = expectedSeq[r]
                          /\ TryAppend(r)
                       \/ /\ l.outcome = "conflict"
                          /\ Len(streams[id]) # expectedSeq[r]
                          /\ TryAppend(r)
                       \/ /\ l.outcome = "error"             \* nothing landed
                          /\ Fail(r)
                    /\ Matched(r)

AnswerStep(r) == /\ IsNext(r, "Answer")
                 /\ \/ /\ answer[r] = None
                       /\ Line(r).outcome = Failed
                       /\ Fail(r)
                    \/ /\ answer[r] # None
                       /\ answer[r] = Line(r).outcome
                       /\ UNCHANGED vars
                 /\ Matched(r)

TraceNext == \E r \in TraceRequests:
               \/ SubmitStep(r)
               \/ ReceiveStep(r)
               \/ LoadStep(r)
               \/ AppendStep(r)
               \/ AnswerStep(r)

----------------------------------------------------------------------------

(* Every line is one step and every Request one Submit, so a behavior that
   matches the whole trace is exactly this many states long, and none that
   stops short is. *)
TraceAccepted ==
  LET want  == 1 + Cardinality(TraceRequests) + Len(Log)
      depth == TLCGet("stats").diameter
  IN \/ depth = want
     \/ /\ PrintT(<<"REJECTED", TraceFile, "no behavior of eventstore matches past depth",
                   depth, "of", want>>)
        /\ FALSE

=============================================================================
