# frozen_string_literal: true
# typed: strict

module Services
  module Securities
    # How far a Security has got through its life, told as its phases in order:
    # offering, funded, drawn, repaying, repaid. Read off Ruby's record and the
    # projections, so it lags the ledger exactly as they do.
    #
    # The counterpart of Services::Ach::Progress, and the same shape: a pure
    # function over a snapshot the caller assembles, so it can be reasoned
    # about and tested without a database. GraphQL exposes it as
    # `Security.stages`, and clients render it rather than re-deriving it from
    # raw states.
    #
    # The first phase that failed ends the walk: a Security whose offering was
    # rejected never reaches the rest.
    module Stage
      # A phase, in the order a Security reaches them.
      class Name < T::Enum
        enums do
          Offering = new('offering')   # supply minted; claims are for sale
          Funded = new('funded')       # every claim sold
          Drawn = new('drawn')         # the escrow moved to the Borrower
          Repaying = new('repaying')   # the Borrower has begun paying it back
          Repaid = new('repaid')       # no principal outstanding
        end
      end

      # Where a phase stands.
      class Status < T::Enum
        enums do
          Done = new('done')
          Waiting = new('waiting')
          Failed = new('failed')
          Skipped = new('skipped') # an earlier phase failed, so this one never will run
        end
      end

      # One phase and where it stands.
      class Step < T::Struct
        const :name, Name
        const :status, Status
      end

      TransactionState = Types::Enums::TransactionState

      # What Ruby last saw of a Security. Each state is nil until the
      # projection has seen that Transaction — an offering whose supply mint
      # was never sent, or never observed, is indistinguishable here, and both
      # mean the same thing to a reader: it has not opened.
      class Snapshot < T::Struct
        const :offering_state, T.nilable(TransactionState)
        const :draw_state, T.nilable(TransactionState)
        # The offering size, and what of it has been sold.
        const :principal_minor_units, Integer
        const :subscribed_minor_units, Integer
        # What is still owed across every holder, once drawn.
        const :outstanding_principal_minor_units, Integer
        # Whether any Repayment has been recorded against it.
        const :repaid_anything, T::Boolean

        sig { returns(T::Boolean) }
        def fully_subscribed? = subscribed_minor_units >= principal_minor_units
      end

      FAILED = T.let(
        [TransactionState::Rejected, TransactionState::RollbackStarted,
         TransactionState::RolledBack, TransactionState::RollbackFailed].freeze,
        T::Array[TransactionState]
      )

      ORDER = T.let(
        [Name::Offering, Name::Funded, Name::Drawn, Name::Repaying, Name::Repaid].freeze, T::Array[Name]
      )

      # Every phase and where it stands, in order.
      sig { params(snapshot: Snapshot).returns(T::Array[Step]) }
      def self.of(snapshot)
        skip_after_failure(ORDER.map { |name| Step.new(name: name, status: status_of(name, snapshot)) })
      end

      # The furthest phase reached, which is what a list or a badge shows.
      # Offering until something is actually done, so a Security that has not
      # opened yet still reads as an offering rather than as nothing.
      #
      # Deliberately coarse: it cannot tell a Security whose Supply was never
      # minted from one that is minted and selling, because both are "no phase
      # done yet" and a reader does not care. Anything that must tell them
      # apart asks `open_for_subscription?` instead.
      sig { params(snapshot: Snapshot).returns(Name) }
      def self.current(snapshot)
        done = of(snapshot).select { |step| step.status == Status::Done }
        done.last&.name || Name::Offering
      end

      # Whether claims can still be bought: the Supply has been minted, and
      # not all of it has been sold.
      #
      # Advisory, like everything read off the projections. The Supply wallet
      # is what actually refuses an oversubscription; this only decides whether
      # asking is worth the round trip and what to say when it is not.
      sig { params(snapshot: Snapshot).returns(T::Boolean) }
      def self.open_for_subscription?(snapshot)
        status_of(Name::Offering, snapshot) == Status::Done && !snapshot.fully_subscribed?
      end

      sig { params(name: Name, snapshot: Snapshot).returns(Status) }
      def self.status_of(name, snapshot)
        case name
        when Name::Offering then from_transaction(snapshot.offering_state)
        when Name::Funded then snapshot.fully_subscribed? ? Status::Done : Status::Waiting
        when Name::Drawn then from_transaction(snapshot.draw_state)
        when Name::Repaying then snapshot.repaid_anything ? Status::Done : Status::Waiting
        else repaid_status(snapshot)
        end
      end
      private_class_method :status_of

      # Repaid only once the Borrower has actually paid something back: a
      # Security nobody has bought into also has nothing outstanding, and
      # calling that repaid would be a lie about a loan that never happened.
      sig { params(snapshot: Snapshot).returns(Status) }
      def self.repaid_status(snapshot)
        return Status::Waiting unless snapshot.repaid_anything
        return Status::Waiting unless snapshot.outstanding_principal_minor_units.zero?

        Status::Done
      end
      private_class_method :repaid_status

      sig { params(state: T.nilable(TransactionState)).returns(Status) }
      def self.from_transaction(state)
        return Status::Waiting if state.nil?
        return Status::Failed if FAILED.include?(state)
        return Status::Done if state == TransactionState::Completed

        Status::Waiting
      end
      private_class_method :from_transaction

      # Everything after the first failure is skipped: it never will run.
      sig { params(steps: T::Array[Step]).returns(T::Array[Step]) }
      def self.skip_after_failure(steps)
        failed = steps.index { |step| step.status == Status::Failed }
        return steps if failed.nil?

        steps.each_with_index.map do |step, index|
          index > failed ? Step.new(name: step.name, status: Status::Skipped) : step
        end
      end
      private_class_method :skip_after_failure

      # Assembles the snapshot for `security` from the projections and the
      # read model. The only method here that touches the database — everything
      # above is a pure function of what it returns.
      sig { params(security: Models::Security).returns(Snapshot) }
      def self.snapshot_of(security)
        from(security, Positions.of(security),
             offering: projected(security.offering_transaction_id),
             draw: projected(security.draw_transaction_id))
      end

      # The same, from what a caller already has: a row that arrived with its
      # projections joined on, and the Positions it already loaded. Lets a
      # GraphQL type answer `stage` without re-reading either, and keeps
      # Snapshot's fields the business layer's business rather than the type's.
      sig do
        params(security: Models::Security, positions: T::Array[Positions::Position],
               offering: T.nilable(TransactionState), draw: T.nilable(TransactionState)).returns(Snapshot)
      end
      def self.from(security, positions, offering:, draw:)
        Snapshot.new(
          offering_state: offering,
          draw_state: draw,
          principal_minor_units: security.principal_minor_units,
          subscribed_minor_units: positions.sum(&:principal_minor_units),
          outstanding_principal_minor_units: positions.sum(&:outstanding_principal_minor_units),
          repaid_anything: Models::Repayment.where(security_id: security.id).any?
        )
      end

      sig { params(transaction_id: T.nilable(String)).returns(T.nilable(TransactionState)) }
      def self.projected(transaction_id)
        return nil if transaction_id.nil?

        state = Models::TransactionProjection[transaction_id]&.state
        state && TransactionState.try_deserialize(state)
      end
      private_class_method :projected
    end
  end
end
