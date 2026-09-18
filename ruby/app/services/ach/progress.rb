# frozen_string_literal: true
# typed: strict

module Services
  module Ach
    # How far an ACH Transaction has got through its lifecycle, told as the
    # steps in ruby/CONTEXT.md: initiation, funding (a withdrawal only),
    # submission, settlement, completion and clearing (a deposit only). Read
    # off Ruby's record and the projections, so it lags the ledger exactly as
    # they do.
    #
    # The first step that failed ends the walk: every step after it is skipped,
    # because a returned or rolled-back Transaction never reaches them. A
    # Transaction being rolled back gets one more step, the rollback itself,
    # which can wait days on a reversal (go/docs/adr/0002).
    module Progress
      # A lifecycle step, in the order a Transaction reaches them.
      class Name < T::Enum
        enums do
          Initiation = new('initiation') # Go holds the Transaction
          Funding = new('funding')       # a withdrawal's cleared cash moved to bank control
          Submission = new('submission') # the provider holds the entry
          Settlement = new('settlement') # the real leg posted
          Completion = new('completion') # the shadow leg ran; the Transaction completed
          Clearing = new('clearing')     # the deposit's money moved to cleared cash
          Rollback = new('rollback')     # what had moved was put back
        end
      end

      # Where a step stands.
      class Status < T::Enum
        enums do
          Done = new('done')
          Waiting = new('waiting')
          Failed = new('failed')
          Skipped = new('skipped') # an earlier step failed, so this one never will run
        end
      end

      # One step and where it stands.
      class Step < T::Struct
        const :name, Name
        const :status, Status
      end

      TransactionState = Types::Enums::TransactionState
      TransferState = Types::Enums::TransferState

      # What Ruby last saw of an ACH Transaction: its record and the
      # projections of the Transaction, its two legs and its clearing. Each
      # state is nil until the projection has seen that aggregate.
      class Snapshot < T::Struct
        const :direction, Types::Enums::AchDirection
        # Whether the provider gave a reference for the entry.
        const :submitted, T::Boolean
        const :state, T.nilable(TransactionState)
        const :real_leg_state, T.nilable(TransferState)
        const :shadow_leg_state, T.nilable(TransferState)
        const :clearing_state, T.nilable(TransactionState)

        sig { returns(T::Boolean) }
        def withdrawal? = direction == Types::Enums::AchDirection::Withdrawal
      end

      ROLLING_BACK = T.let(
        [TransactionState::RollbackStarted, TransactionState::RolledBack, TransactionState::RollbackFailed].freeze,
        T::Array[TransactionState]
      )
      CLEARING_FAILED = T.let([TransactionState::Rejected, *ROLLING_BACK].freeze, T::Array[TransactionState])

      # The steps each direction goes through on its way to completing, in order.
      DEPOSIT_STEPS = T.let(
        [Name::Initiation, Name::Submission, Name::Settlement, Name::Completion, Name::Clearing].freeze,
        T::Array[Name]
      )
      WITHDRAWAL_STEPS = T.let(
        [Name::Initiation, Name::Funding, Name::Submission, Name::Settlement, Name::Completion].freeze,
        T::Array[Name]
      )

      class << self
        sig { params(seen: Snapshot).returns(T::Array[Step]) }
        def of(seen)
          steps = skip_after_failure(forward(seen))
          rollback = rollback(seen.state)
          rollback ? [*steps, Step.new(name: Name::Rollback, status: rollback)] : steps
        end

        private

        sig { params(seen: Snapshot).returns(T::Hash[Name, Status]) }
        def forward(seen)
          (seen.withdrawal? ? WITHDRAWAL_STEPS : DEPOSIT_STEPS).to_h { |name| [name, status_of(name, seen)] }
        end

        sig { params(name: Name, seen: Snapshot).returns(Status) }
        def status_of(name, seen)
          case name
          when Name::Initiation then initiation(seen.state, seen.submitted)
          when Name::Funding then leg(seen.shadow_leg_state)
          when Name::Submission then submission(seen.state, seen.real_leg_state, seen.submitted)
          when Name::Settlement then leg(seen.real_leg_state)
          when Name::Completion then completion(seen.state)
          else clearing(seen.clearing_state)
          end
        end

        sig { params(state: T.nilable(TransactionState), submitted: T::Boolean).returns(Status) }
        def initiation(state, submitted)
          return Status::Failed if state == TransactionState::Rejected
          return Status::Done if state || submitted

          Status::Waiting
        end

        # A Transaction rolling back without ever having been submitted is one
        # the provider refused at submission.
        sig do
          params(state: T.nilable(TransactionState), real_leg_state: T.nilable(TransferState),
                 submitted: T::Boolean).returns(Status)
        end
        def submission(state, real_leg_state, submitted)
          return Status::Done if submitted
          return Status::Failed if real_leg_state == TransferState::Cancelled || ROLLING_BACK.include?(state)

          Status::Waiting
        end

        # A leg's step is done once it posts, and failed if it never will: the
        # real leg for settlement, a withdrawal's shadow leg for funding.
        sig { params(leg_state: T.nilable(TransferState)).returns(Status) }
        def leg(leg_state)
          case leg_state
          when TransferState::Committed then Status::Done
          when TransferState::Rejected, TransferState::Cancelled, TransferState::Failed then Status::Failed
          else Status::Waiting
          end
        end

        sig { params(state: T.nilable(TransactionState)).returns(Status) }
        def completion(state)
          return Status::Done if state == TransactionState::Completed
          return Status::Failed if ROLLING_BACK.include?(state)

          Status::Waiting
        end

        sig { params(clearing_state: T.nilable(TransactionState)).returns(Status) }
        def clearing(clearing_state)
          return Status::Done if clearing_state == TransactionState::Completed
          return Status::Failed if CLEARING_FAILED.include?(clearing_state)

          Status::Waiting
        end

        # Nil while nothing is being rolled back.
        sig { params(state: T.nilable(TransactionState)).returns(T.nilable(Status)) }
        def rollback(state)
          case state
          when TransactionState::RollbackStarted then Status::Waiting
          when TransactionState::RolledBack then Status::Done
          when TransactionState::RollbackFailed then Status::Failed
          end
        end

        sig { params(statuses: T::Hash[Name, Status]).returns(T::Array[Step]) }
        def skip_after_failure(statuses)
          failed = T.let(false, T::Boolean)
          statuses.map do |name, status|
            step = Step.new(name: name, status: failed ? Status::Skipped : status)
            failed ||= status == Status::Failed
            step
          end
        end
      end
    end
  end
end
