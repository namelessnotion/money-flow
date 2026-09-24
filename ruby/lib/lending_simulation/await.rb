# frozen_string_literal: true
# typed: strict

module LendingSimulation
  # Waits for the read model to see what the simulation started. Every call
  # into the platform only has Go *accept* the work; the outcome arrives later
  # through the projections, and the next step of a flow depends on it — the
  # Ruby counterpart of go/cmd/simulate's wait.go.
  #
  # Polls in batches: a day's worth of Subscriptions is started together and
  # awaited together, one query per poll rather than one per Transaction.
  class Await
    # What the read model still had not seen when time ran out, by id with the
    # last state it did see — enough to go and look.
    class TimedOut < StandardError; end

    # A Transaction ended somewhere other than completed.
    class Failed < StandardError; end

    TransactionState = Types::Enums::TransactionState
    TransferState = Types::Enums::TransferState

    TERMINAL = T.let(
      [TransactionState::Completed, TransactionState::Rejected, TransactionState::RolledBack,
       TransactionState::RollbackFailed].map(&:serialize).freeze,
      T::Array[String]
    )

    # A Transfer that can never reach the state awaited.
    DEAD_END = T.let([TransferState::Rejected, TransferState::Failed, TransferState::Cancelled].map(&:serialize).freeze,
                     T::Array[String])

    sig { params(timeout: Float, interval: Float).void }
    def initialize(timeout: 60.0, interval: 0.05)
      @timeout = timeout
      @interval = interval
    end

    # Every Transaction completed; raises Failed naming any that ended
    # otherwise, with its reason.
    sig { params(ids: T::Array[String]).void }
    def completed!(ids)
      failed = settled(ids).reject { |_id, state| state == TransactionState::Completed.serialize }
      return if failed.empty?

      reasons = DB[:transaction_projections].where(aggregate_id: failed.keys).select_hash(:aggregate_id, :reason)
      raise Failed, failed.map { |id, state| "#{id} #{state}: #{reasons[id]}" }.join('; ')
    end

    # Each Transaction's terminal state, once the read model has seen them all
    # reach one.
    sig { params(ids: T::Array[String]).returns(T::Hash[String, String]) }
    def settled(ids)
      poll(:transaction_projections, ids) { |state| TERMINAL.include?(state) }
    end

    # Every Transfer has reached `state`. A Transfer that dead-ends instead
    # raises Failed rather than being waited on until the timeout.
    sig { params(ids: T::Array[String], state: Types::Enums::TransferState).void }
    def transfers!(ids, state:)
      seen = poll(:transfer_projections, ids) { |current| current == state.serialize || DEAD_END.include?(current) }
      dead = seen.reject { |_id, current| current == state.serialize }
      raise Failed, "transfers never reached #{state.serialize}: #{dead}" unless dead.empty?
    end

    private

    sig do
      params(table: Symbol, ids: T::Array[String], done: T.proc.params(state: String).returns(T::Boolean))
        .returns(T::Hash[String, String])
    end
    def poll(table, ids, &done)
      deadline = now + @timeout
      loop do
        seen = states(table, ids)
        pending = ids.reject { |id| (state = seen[id]) && done.call(state) }
        return seen if pending.empty?
        raise TimedOut, "#{table}: #{described(pending, seen)}" if now > deadline

        sleep(@interval)
      end
    end

    sig { returns(Float) }
    def now = Float(Process.clock_gettime(Process::CLOCK_MONOTONIC))

    sig { params(table: Symbol, ids: T::Array[String]).returns(T::Hash[String, String]) }
    def states(table, ids)
      DB[table].where(aggregate_id: ids).select_map(%i[aggregate_id state]).to_h do |id, state|
        [String(id), String(state)]
      end
    end

    # The first few still waiting, each with the last state seen.
    sig { params(pending: T::Array[String], seen: T::Hash[String, String]).returns(String) }
    def described(pending, seen)
      shown = pending.first(5).map { |id| "#{id} (#{seen.fetch(id, 'not seen')})" }
      pending.size > 5 ? "#{shown.join(', ')} and #{pending.size - 5} more" : shown.join(', ')
    end
  end
end
