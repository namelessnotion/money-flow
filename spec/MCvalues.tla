------------------------------ MODULE MCvalues ------------------------------
(* The domain values both TLC models use, kept in one place so MCeventlog
   checks eventlog's promises under exactly the values MCeventstore checks
   eventstore's refinement under. A .cfg can't define a function-valued
   constant like Handlers, so they live in a module the .cfg files
   substitute from. *)

MCIds == {"t1", "t2"}

MCCommands == {"Open", "Close"}

MCEvents == {"Opened", "OpenRejected", "Closed", "CloseRejected"}

MCHandlers == [c \in MCCommands |->
                 IF c = "Open" THEN {"Opened", "OpenRejected"}
                               ELSE {"Closed", "CloseRejected"}]

=============================================================================
