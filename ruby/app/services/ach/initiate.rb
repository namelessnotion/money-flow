# frozen_string_literal: true
# typed: strict

require 'securerandom'
require_relative '../base_service'
require_relative 'fake_provider'
require_relative 'go_gateway'
require_relative 'return'
require_relative 'transaction_shape'

module Services
  module Ach
    # Originates an ACH deposit or withdrawal for an entity.
    #
    # 1. Records the intent — direction, amount and every id — before calling
    #    anything, so a retry or a re-run after a crash resends the same ids and
    #    converges on Go's idempotency instead of originating twice.
    # 2. Asks Go to run the Transaction. Its real leg stops, staged — after,
    #    for a withdrawal, the cleared cash has moved to bank control.
    # 3. Asks Go how the Transaction stands, and goes on only if it is still
    #    running: a withdrawal short of cleared cash is rolled back inside
    #    step 2, and must never reach the provider (ruby/docs/adr/0004).
    # 4. Submits the entry to the provider, then confirms the staged real leg:
    #    it now waits, pending, for the ACH network to settle or return it.
    #
    # Lifecycle state is not recorded here: it arrives through the projection.
    class Initiate < BaseService
      sig { params(gateway: GoGateway, provider: Provider).void }
      def initialize(gateway: GoGateway.new, provider: FakeProvider.new)
        super()
        @go = gateway
        @provider = provider
      end

      sig { params(request: InitiateAchRequest).returns(Models::AchTransaction) }
      def call(request:)
        raise InvalidAmount, 'amount must be positive' unless request.amount_minor_units.positive?

        shape = plan(request)
        ach = perform { record_intent(request, shape) }

        start_transaction!(shape, ach)
        require_running!(ach)
        submit(ach, shape)
        ach
      end

      private

      # Go's own accept/reject decision (go/docs/adr/0004) now catches the
      # common underfunded-withdrawal case here, before any event exists —
      # not just a malformed DAG. Re-wraps GoGateway's bare refusal with the
      # same framing require_running!'s own, rarer catch uses below, so the
      # message a caller sees doesn't depend on which of the two checks
      # caught it.
      sig { params(shape: TransactionShape, ach: Models::AchTransaction).void }
      def start_transaction!(shape, ach)
        @go.start_transaction(shape.start_request(transaction_id: ach.id, amount_minor_units: ach.amount_minor_units))
      rescue Refused => e
        raise Refused, "refused before any money moved: #{e.message}"
      end

      # Nothing has left the platform yet, so a Transaction Go did not keep
      # running is simply refused. This is the only thing that ever learns
      # the real dispatch's actual outcome — start_transaction!'s own
      # response is built before Go's saga runs, so it can catch the common
      # case early but can never be the last word (go/docs/adr/0004).
      sig { params(ach: Models::AchTransaction).void }
      def require_running!(ach)
        outcome = @go.resume(ach.id)
        return if outcome.started?

        raise Refused, "refused before any money moved: #{outcome.reason.empty? ? outcome.state : outcome.reason}"
      end

      sig { params(request: InitiateAchRequest).returns(TransactionShape) }
      def plan(request)
        raise NotFound, "no entity #{request.entity_id}" if Models::Entity[request.entity_id].nil?

        accounts = Models::Account.where(entity_id: request.entity_id).all
        TransactionShape.for(direction: request.direction, accounts: accounts,
                             real_transfer_id: SecureRandom.uuid_v7, shadow_transfer_id: SecureRandom.uuid_v7)
      end

      sig { params(request: InitiateAchRequest, shape: TransactionShape).returns(Models::AchTransaction) }
      def record_intent(request, shape)
        Models::AchTransaction.create(
          id: SecureRandom.uuid_v7,
          entity_id: request.entity_id,
          direction: request.direction.serialize,
          amount_minor_units: request.amount_minor_units,
          currency: TransactionShape::CURRENCY,
          real_transfer_id: shape.real_transfer_id,
          shadow_transfer_id: shape.shadow_transfer_id
        )
      end

      # The reference is kept as soon as the provider gives it, before Go is
      # told: an entry the provider holds must always be traceable.
      sig { params(ach: Models::AchTransaction, shape: TransactionShape).void }
      def submit(ach, shape)
        reference = submit_entry(ach, shape)
        perform { ach.update(provider_reference: reference) }
        @go.confirm_staged(ach.real_transfer_id)
      end

      # A refused submission is a return that happened early: the entry will
      # never post, so the Transaction is rolled back the same way.
      sig { params(ach: Models::AchTransaction, shape: TransactionShape).returns(String) }
      def submit_entry(ach, shape)
        @provider.submit(entry(ach, shape))
      rescue Provider::SubmissionFailed => e
        reason = "provider refused the entry: #{e.message}"
        Return.new(gateway: @go).call(ach_transaction_id: ach.id, reason: reason)
        raise Refused, reason
      end

      sig { params(ach: Models::AchTransaction, shape: TransactionShape).returns(Entry) }
      def entry(ach, shape)
        Entry.new(
          transaction_id: ach.id,
          direction: shape.direction,
          amount_minor_units: ach.amount_minor_units,
          currency: ach.currency,
          bank_wallet_id: shape.wallets.fetch(Types::Enums::AccountType::Bank)
        )
      end
    end
  end
end
