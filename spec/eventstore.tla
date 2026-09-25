----------------------------- MODULE eventstore -----------------------------
(* The design that keeps eventlog's promises: several Workers handle Requests
   at once, the way the Twirp handlers in go/internal/* do (e.g.
   RequestTransfer in go/internal/transfer/server.go):

     1. Load the aggregate's stream;
     2. if the stream already holds one of the command's events, answer
        with it; otherwise decide an event against what was loaded;
     3. tryAppend it at ExpectedSeq = the length that was loaded.

   Nothing but the database stops two Workers from interleaving between 1
   and 3. Postgres's UNIQUE(aggregate_type, aggregate_id, sequence)
   (go/db/migrations/00001_create_events.up.sql) makes a stale ExpectedSeq
   fail with ErrConcurrencyConflict, and the Worker reloads and re-decides,
   giving up with twirp.Aborted after MaxAttempts (maxConcurrencyAttempts).

   The environment is adversarial where the real one is: Requests arrive in
   any order, a Worker can fail at any point in its command (twirp.Internal),
   and a COMMIT can land while its reply is lost.

   Nothing here restates eventlog's promises. Refines checks that every step
   this design takes is a step eventlog allows, with streams as eventlog's
   log and Aborted/Failed as its Unknown, so this module shares no
   definitions with the one it is checked against. *)
EXTENDS Naturals, Sequences

CONSTANTS Ids, Commands, Events, Handlers, Requests, None, Workers, MaxAttempts

Aborted == "Aborted"  \* twirp.Aborted: lost MaxAttempts races
Failed  == "Failed"   \* twirp.Internal: may or may not have decided the command

VARIABLES request,      \* as in eventlog: what each Request asked for
          answer,       \* what each Request was told: an event, Aborted or Failed
          inbox,        \* Requests submitted but not yet received by a Worker
          streams,      \* each aggregate's event stream, indexed by sequence
          pc,           \* where each Worker is in the Load/tryAppend loop
          handling,     \* the Request each Worker is handling
          expectedSeq,  \* the stream length each Worker loaded
          decision,     \* the event each Worker decided on that load
          attempts      \* conflicts each Worker has lost on this Request

vars == <<request, answer, inbox, streams, pc, handling, expectedSeq,
          decision, attempts>>

Contract == INSTANCE eventlog
              WITH log    <- streams,
                   answer <- [r \in Requests |->
                                IF answer[r] \in {Aborted, Failed}
                                   THEN "Unknown"  \* eventlog's Unknown
                                   ELSE answer[r]]

Refines == Contract!Spec

(* Refines without its liveness, which is all a finite trace can be checked
   against (Traceeventstore) *)
RefinesSafety == Contract!Init /\ [][Contract!Next]_Contract!vars

(* TLC doesn't check an instantiated module's ASSUMEs, so eventlog's is
   asserted again here *)
ASSUME /\ Contract!HandlersAreWellFormed
       /\ MaxAttempts \in Nat \ {0}
       /\ {Aborted, Failed} \cap Events = {}

TypeOK == /\ request \in [Requests -> (Ids \X Commands) \cup {None}]
          /\ answer \in [Requests -> Events \cup {Aborted, Failed, None}]
          /\ inbox \subseteq Requests
          /\ DOMAIN streams = Ids
          /\ \A id \in Ids: \A i \in 1..Len(streams[id]): streams[id][i] \in Events
          /\ pc \in [Workers -> {"idle", "load", "append"}]
          /\ handling \in [Workers -> Requests \cup {None}]
          /\ expectedSeq \in [Workers -> Nat]
          /\ decision \in [Workers -> Events \cup {None}]
          /\ attempts \in [Workers -> 0..(MaxAttempts - 1)]
          (* a Worker holds a Request exactly while it isn't idle, and a
             decision exactly while it's about to append *)
          /\ \A w \in Workers: /\ (pc[w] = "idle") = (handling[w] = None)
                               /\ (pc[w] = "append") = (decision[w] # None)

(* a submitted Request is in exactly one place: the inbox, one Worker's
   hands, or answered *)
EachRequestInOnePlace ==
  \A r \in Requests:
    request[r] # None =>
      LET inInbox    == r \in inbox
          held       == {w \in Workers : handling[w] = r}
          isAnswered == answer[r] # None
      IN \/ inInbox /\ held = {} /\ ~isAnswered
         \/ ~inInbox /\ \E w \in Workers: held = {w} /\ ~isAnswered
         \/ ~inInbox /\ held = {} /\ isAnswered

Init == /\ request = [r \in Requests |-> None]
        /\ answer = [r \in Requests |-> None]
        /\ inbox = {}
        /\ streams = [id \in Ids |-> << >>]
        /\ pc = [w \in Workers |-> "idle"]
        /\ handling = [w \in Workers |-> None]
        /\ expectedSeq = [w \in Workers |-> 0]
        /\ decision = [w \in Workers |-> None]
        /\ attempts = [w \in Workers |-> 0]

Submit(r) == /\ request[r] = None
             /\ \E id \in Ids, c \in Commands: request' = [request EXCEPT ![r] = <<id, c>>]
             /\ inbox' = inbox \cup {r}
             /\ UNCHANGED <<answer, streams, pc, handling, expectedSeq, decision, attempts>>

(* w is done with its Request and ready for the next *)
Finish(w) == /\ pc' = [pc EXCEPT ![w] = "idle"]
             /\ handling' = [handling EXCEPT ![w] = None]
             /\ expectedSeq' = [expectedSeq EXCEPT ![w] = 0]
             /\ decision' = [decision EXCEPT ![w] = None]
             /\ attempts' = [attempts EXCEPT ![w] = 0]

Receive(w) == /\ pc[w] = "idle"
              /\ \E r \in inbox: /\ handling' = [handling EXCEPT ![w] = r]
                                 /\ inbox' = inbox \ {r}
              /\ pc' = [pc EXCEPT ![w] = "load"]
              /\ UNCHANGED <<request, answer, streams, expectedSeq, decision, attempts>>

(* Load the stream and look for this command's own event types. If there is
   none, decide: nondeterministically, because in the real handlers it
   depends on state outside this stream (a wallet's balance, another
   aggregate) that can change between one attempt and the next. *)
Load(w) == LET r    == handling[w]
               id   == request[r][1]
               c    == request[r][2]
               s    == streams[id]
               mine == {i \in 1..Len(s) : s[i] \in Handlers[c]}
           IN /\ pc[w] = "load"
              /\ IF mine # {}
                    THEN /\ answer' = [answer EXCEPT ![r] = s[CHOOSE i \in mine: TRUE]]
                         /\ Finish(w)
                    ELSE /\ \E e \in Handlers[c]: decision' = [decision EXCEPT ![w] = e]
                         /\ expectedSeq' = [expectedSeq EXCEPT ![w] = Len(s)]
                         /\ pc' = [pc EXCEPT ![w] = "append"]
                         /\ UNCHANGED <<handling, attempts, answer>>
              /\ UNCHANGED <<request, inbox, streams>>

(* The INSERT at sequence expectedSeq + 1 either lands or collides with a row
   another Worker already wrote there: the database does this check and the
   write as one step, which is what makes it safe to take in one action.
   When it lands, the reply can still be lost (pgx's Commit erroring after
   the server committed), and the caller sees twirp.Internal. *)
TryAppend(w) == LET r  == handling[w]
                    id == request[r][1]
                IN /\ pc[w] = "append"
                   /\ IF Len(streams[id]) = expectedSeq[w]
                         THEN /\ streams' = [streams EXCEPT ![id] = Append(@, decision[w])]
                              /\ \/ answer' = [answer EXCEPT ![r] = decision[w]]
                                 \/ answer' = [answer EXCEPT ![r] = Failed]
                              /\ Finish(w)
                         ELSE /\ UNCHANGED streams
                              /\ IF attempts[w] + 1 < MaxAttempts
                                    THEN /\ attempts' = [attempts EXCEPT ![w] = @ + 1]
                                         /\ pc' = [pc EXCEPT ![w] = "load"]
                                         /\ expectedSeq' = [expectedSeq EXCEPT ![w] = 0]
                                         /\ decision' = [decision EXCEPT ![w] = None]
                                         /\ UNCHANGED <<handling, answer>>
                                    ELSE /\ answer' = [answer EXCEPT ![r] = Aborted]
                                         /\ Finish(w)
                   /\ UNCHANGED <<request, inbox>>

(* the Load, the decision or the INSERT fails without anything landing *)
Fail(w) == /\ pc[w] \in {"load", "append"}
           /\ answer' = [answer EXCEPT ![handling[w]] = Failed]
           /\ Finish(w)
           /\ UNCHANGED <<request, inbox, streams>>

(* as in eventlog: stopping after the last answer is not a deadlock *)
Done == /\ \A r \in Requests: answer[r] # None
        /\ UNCHANGED vars

Next == \/ \E r \in Requests: Submit(r)
        \/ \E w \in Workers: Receive(w) \/ Load(w) \/ TryAppend(w) \/ Fail(w)
        \/ Done

(* the server keeps taking Requests and working the ones it holds; nothing
   forces a failure *)
Fairness == \A w \in Workers: /\ WF_vars(Receive(w))
                              /\ WF_vars(Load(w))
                              /\ WF_vars(TryAppend(w))

Spec == Init /\ [][Next]_vars /\ Fairness

THEOREM Spec => /\ [](TypeOK /\ EachRequestInOnePlace)
                /\ Refines

=============================================================================
