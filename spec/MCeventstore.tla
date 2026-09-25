---------------------------- MODULE MCeventstore ----------------------------
(* TLC model checking that eventstore refines eventlog, under the same
   values MCeventlog checks eventlog's promises under. *)
EXTENDS eventstore, MCvalues

(* one below maxConcurrencyAttempts (3, go/internal/transfer/server.go). A
   Worker only loses a race to another decision landing on the same stream,
   and with two commands per stream it can lose at most two, so at 3 the
   Aborted branch would never be explored *)
MCMaxAttempts == 2

=============================================================================
