------------------------------ MODULE eventlog ------------------------------
(* What the event log promises, stated without any of the machinery that
   keeps the promise. Callers submit Requests, each asking for a command on
   an aggregate id; readers (projections, the orchestrator) read each id's
   stream. The promises:

   - a command is decided at most once per id, by appending one of its
     handler's events, and only because some caller asked for it;
   - the log is append-only;
   - a caller is told either the decision the log records for its command,
     or Unknown, which promises nothing about the log (the caller retries);
   - every request is eventually answered.

   This is the specification the design is verified against: eventstore.tla
   (the concurrent Load/tryAppend handlers in go/internal) is checked to
   refine it, so a flaw in the design shows up as a broken promise here
   rather than as a property restated in the design's own terms. *)
EXTENDS Naturals, Sequences

CONSTANTS Ids, Commands, Events, Handlers, Requests, None

(* a caller told Unknown knows nothing about whether its command was
   decided: twirp.Aborted and twirp.Internal both mean this *)
Unknown == "Unknown"

(* no command can have a possible empty set of events, that is all commands
   must result in atleast 1 event; and no two commands share an event, so an
   event in the log says which command it decided, as each command's
   rejection does in proto/ (TransferRequestRejected, ...) *)
HandlersAreWellFormed ==
  /\ \A c \in Commands: Handlers[c] \subseteq Events /\ Handlers[c] # {}
  /\ \A c, d \in Commands: (c # d) => Handlers[c] \cap Handlers[d] = {}
  /\ Unknown \notin Events

ASSUME HandlersAreWellFormed

VARIABLES log,      \* each aggregate id's stream of events
          request,  \* the <<id, command>> each Request asked for, None until submitted
          answer    \* what each Request was told, None while it waits

vars == <<log, request, answer>>

HandledEvents == UNION {Handlers[c] : c \in Commands}

(* the positions in stream s holding c's decision *)
DecidedAt(s, c) == {i \in 1..Len(s) : s[i] \in Handlers[c]}

TypeOK == /\ log \in [Ids -> Seq(HandledEvents)]
          /\ request \in [Requests -> (Ids \X Commands) \cup {None}]
          /\ answer \in [Requests -> HandledEvents \cup {Unknown, None}]
          /\ \A r \in Requests: (request[r] = None) => (answer[r] = None)

Init == /\ log = [id \in Ids |-> << >>]
        /\ request = [r \in Requests |-> None]
        /\ answer = [r \in Requests |-> None]

Waiting(r) == request[r] # None /\ answer[r] = None

Submit(r) == /\ request[r] = None
             /\ \E id \in Ids, c \in Commands: request' = [request EXCEPT ![r] = <<id, c>>]
             /\ UNCHANGED <<log, answer>>

(* r's command is undecided, so it is decided now: one of its handler's
   events is appended, and r is told which, or Unknown if the reply is lost *)
Decide(r) == LET id == request[r][1]
                 c  == request[r][2]
             IN /\ Waiting(r)
                /\ DecidedAt(log[id], c) = {}
                /\ \E e \in Handlers[c]:
                     /\ log' = [log EXCEPT ![id] = Append(@, e)]
                     /\ answer' \in {[answer EXCEPT ![r] = e], [answer EXCEPT ![r] = Unknown]}
                /\ UNCHANGED request

(* r's command was already decided, and r is told what it was decided as *)
Replay(r) == LET id == request[r][1]
                 c  == request[r][2]
             IN /\ Waiting(r)
                /\ \E i \in DecidedAt(log[id], c): answer' = [answer EXCEPT ![r] = log[id][i]]
                /\ UNCHANGED <<log, request>>

(* r is told Unknown, and nothing is decided *)
GiveUp(r) == /\ Waiting(r)
             /\ answer' = [answer EXCEPT ![r] = Unknown]
             /\ UNCHANGED <<log, request>>

(* Requests is finite, so there is a last answer; after it nothing happens,
   and TLC is told so here rather than having deadlock checking switched
   off, which would hide a state stuck with a request still waiting *)
Done == /\ \A r \in Requests: answer[r] # None
        /\ UNCHANGED vars

Next == \/ \E r \in Requests: Submit(r) \/ Decide(r) \/ Replay(r) \/ GiveUp(r)
        \/ Done

Spec == /\ Init /\ [][Next]_vars
        /\ \A r \in Requests: WF_vars(Decide(r) \/ Replay(r) \/ GiveUp(r))

----------------------------------------------------------------------------
(* The promises, written without DecidedAt so that checking them on this
   module (MCeventlog) tests the actions above, not the other way round. *)

NoDuplicateEventsInLog ==
  \A id \in Ids: \A i, j \in 1..Len(log[id]): (i # j) => log[id][i] # log[id][j]

AtMostOneDecisionPerCommand ==
  \A id \in Ids, c \in Commands: \A i, j \in 1..Len(log[id]):
    (i # j) => ~(log[id][i] \in Handlers[c] /\ log[id][j] \in Handlers[c])

(* a definite answer is one of the caller's own command's events, and it is
   in the log *)
AnswersAreRecordedDecisions ==
  \A r \in Requests: answer[r] \in Events =>
    LET id == request[r][1]
        c  == request[r][2]
    IN /\ answer[r] \in Handlers[c]
       /\ \E i \in 1..Len(log[id]): log[id][i] = answer[r]

(* callers asking for the same command are never told different things *)
AnswersAgree ==
  \A r1, r2 \in Requests:
    (request[r1] = request[r2] /\ answer[r1] \in Events /\ answer[r2] \in Events)
      => answer[r1] = answer[r2]

(* go/db/migrations/00001_create_events.up.sql forbids UPDATE and DELETE *)
AppendOnly ==
  [][\A id \in Ids: /\ Len(log'[id]) >= Len(log[id])
                    /\ SubSeq(log'[id], 1, Len(log[id])) = log[id]]_log

EveryRequestIsAnswered ==
  \A r \in Requests: (request[r] # None) ~> (answer[r] # None)

THEOREM Spec => /\ [](/\ TypeOK
                      /\ NoDuplicateEventsInLog
                      /\ AtMostOneDecisionPerCommand
                      /\ AnswersAreRecordedDecisions
                      /\ AnswersAgree)
                /\ AppendOnly
                /\ EveryRequestIsAnswered

=============================================================================
