# frozen_string_literal: true
# typed: strict

require_relative '../base_service'
require_relative 'entry'
require_relative 'fake_provider'
require_relative 'go_gateway'
require_relative 'return'
require_relative 'transaction_shape'

module Services
  module Ach
    # Hands one ACH entry to the provider, and confirms the staged real leg once
    # the provider holds it.
    #
    # This used to be the tail of Initiate. It cannot be any more: since the
    # async cutover (go/docs/adr/0006) Initiate's call to Go returns with the
    # Transaction merely accepted, so at that moment a withdrawal's funding leg
    # has not run and there is nothing yet to be sure of. Submission has to
    # happen once the ledger has actually moved, which is why it is a sweep
    # (SubmitDue) rather than a step.
    #
    # **The guarantee ruby/docs/adr/0004 exists for is preserved here, and it is
    # stronger than it was.** Money must never leave over ACH unless the
    # withdrawal is funded, and this asks Go — not the read model — immediately
    # before handing the entry over. Two things have to hold:
    #
    # 1. The real leg is staged. For a withdrawal that is only reachable past
    #    the dependency edge on the funding leg, so it *proves* funding
    #    happened rather than inferring it from the Transaction merely being
    #    started and not yet terminal, which is all the old single check could
    #    do. For a deposit the real leg is the DAG root and staging is its own
    #    first step, so the same condition reads correctly for both directions.
    # 2. Go says the Transaction is still STARTED. A past fact is not enough;
    #    only a live, authoritative read rules out something having gone wrong
    #    since. The residual window between that read and the provider call is
    #    unchanged from before, and ADR 0004 decision 3 already covers it — the
    #    funding is undone by an ordinary, unstaged reversal.
    #
    # The provider reference is persisted as soon as the provider gives it, and
    # before Go is told anything: an entry the provider holds must always be
    # traceable. It is also the record that this entry was sent — no
    # "submitted" flag, the same way `disbursements` keeps no "already
    # disbursed" one — so a row carrying it is never handed to the provider
    # again. Go can still fail after it is saved, though, which leaves the
    # provider holding an entry whose real leg is still staged; while the read
    # model sees it staged, each call finishes that confirmation instead.
    #
    # Unlike every other guard in this codebase, that one is Ruby's rather than
    # Go's, and it has to be: the effect is outside the ledger, so Go cannot
    # dedupe it. Resubmission safety is the Provider port's own contract — the
    # entry carries the Transaction id as its idempotency key.
    class Submit < BaseService
      sig { params(gateway: GoGateway, provider: Provider).void }
      def initialize(gateway: GoGateway.new, provider: FakeProvider.new)
        super()
        @go = gateway
        @provider = provider
      end

      sig { params(ach_transaction_id: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:)
        ach = Models::AchTransaction[ach_transaction_id] ||
              raise(NotFound, "no ACH transaction #{ach_transaction_id}")
        return finish_confirmation(ach) unless ach.provider_reference.nil?

        require_staged!(ach)
        require_running!(ach)
        submit(ach)
        ach
      end

      private

      # The provider already holds the entry, so the only step that can be left
      # is confirming the real leg. Go answers a leg that is already pending as
      # confirmed, so repeating this while the projection catches up is safe;
      # one the read model has seen move on needs nothing.
      sig { params(ach: Models::AchTransaction).returns(Models::AchTransaction) }
      def finish_confirmation(ach)
        @go.confirm_staged(ach.real_transfer_id) if staged?(ach)
        ach
      end

      sig { params(ach: Models::AchTransaction).returns(T::Boolean) }
      def staged?(ach)
        Models::TransferProjection[ach.real_transfer_id]&.state == Types::Enums::TransferState::Staged.serialize
      end

      # The read model's half of the gate. It may lag Go but it cannot lead it:
      # every state it holds is one Go actually reached, under the projection's
      # monotonic guard (ruby/docs/adr/0001). SubmitDue selects on this already;
      # re-checking here is what makes Submit safe to call on its own, as
      # SubmitNow does.
      sig { params(ach: Models::AchTransaction).void }
      def require_staged!(ach)
        return if staged?(ach)

        state = Models::TransferProjection[ach.real_transfer_id]&.state
        raise NotSubmittable,
              "#{ach.id}'s real leg is #{state || 'not yet seen'}, not staged; nothing may reach the provider yet"
      end

      # Go's half, and the definitive one. STARTED means the Transaction is
      # still running and has not been rolled back out from under the staged
      # leg.
      sig { params(ach: Models::AchTransaction).void }
      def require_running!(ach)
        outcome = @go.state(ach.id)
        return if outcome.started?

        raise NotSubmittable,
              "#{ach.id} is #{outcome.reason.empty? ? outcome.state : outcome.reason}; the entry is not submitted"
      end

      sig { params(ach: Models::AchTransaction).void }
      def submit(ach)
        reference = submit_entry(ach)
        perform { ach.update(provider_reference: reference) }
        @go.confirm_staged(ach.real_transfer_id)
      end

      # A refused submission is a return that happened early: the entry will
      # never post, so the Transaction is rolled back the same way.
      sig { params(ach: Models::AchTransaction).returns(String) }
      def submit_entry(ach)
        @provider.submit(entry(ach))
      rescue Provider::SubmissionFailed => e
        reason = "provider refused the entry: #{e.message}"
        Return.new(gateway: @go).call(ach_transaction_id: ach.id, reason: reason)
        raise Refused, reason
      end

      sig { params(ach: Models::AchTransaction).returns(Entry) }
      def entry(ach)
        Entry.new(
          transaction_id: ach.id,
          direction: direction(ach),
          amount_minor_units: ach.amount_minor_units,
          currency: ach.currency,
          bank_wallet_id: shape(ach).wallets.fetch(Types::Enums::AccountType::Bank)
        )
      end

      # Rebuilt from the row rather than carried over from Initiate: the
      # direction and both leg ids are columns, so the shape this Transaction
      # was sent under is recoverable, and a sweep has no caller to hand it one.
      sig { params(ach: Models::AchTransaction).returns(TransactionShape) }
      def shape(ach)
        TransactionShape.for(
          direction: direction(ach),
          accounts: Models::Account.where(entity_id: ach.entity_id).all,
          real_transfer_id: ach.real_transfer_id,
          shadow_transfer_id: ach.shadow_transfer_id
        )
      end

      sig { params(ach: Models::AchTransaction).returns(Types::Enums::AchDirection) }
      def direction(ach)
        Types::Enums::AchDirection.deserialize(ach.direction)
      end
    end
  end
end
