# frozen_string_literal: true
# typed: strict

require 'securerandom'
require_relative '../base_service'
require_relative 'go_gateway'
require_relative 'transaction_shape'

module Services
  module Ach
    # Originates an ACH deposit or withdrawal for an entity.
    #
    # 1. Records the intent — direction, amount and every id — before calling
    #    anything, so a retry or a re-run after a crash resends the same ids and
    #    converges on Go's idempotency instead of originating twice.
    # 2. Asks Go to accept the Transaction. A withdrawal that cannot be funded
    #    is normally refused right here, by Go's own accept-time pre-flight of
    #    the funding leg, before a single event is written.
    #
    # And that is the whole of it. Nothing is handed to the provider from here.
    #
    # It used to be: this service ran the Transaction, asked Go how it had gone,
    # and submitted the entry once it was sure the funding leg had committed —
    # which is the guarantee ruby/docs/adr/0004 exists for. Since the async
    # cutover (go/docs/adr/0006) Go accepts and returns, so at the end of this
    # method the funding leg has not run and there is nothing yet to be sure of.
    # Asking anyway would only ever get back "initialized", and submitting on
    # that would be paying out a withdrawal nobody had funded.
    #
    # So submission moved to Services::Ach::SubmitDue, a sweep that waits until
    # the ledger has actually moved and then checks with Go before handing the
    # entry over. The guarantee is intact and in fact stronger there; what
    # changes is that a caller here learns only whether the Transaction was
    # accepted, and everything after that arrives through the projection.
    class Initiate < BaseService
      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      sig { params(request: InitiateAchRequest).returns(Models::AchTransaction) }
      def call(request:)
        raise InvalidAmount, 'amount must be positive' unless request.amount_minor_units.positive?

        shape = plan(request)
        ach = perform { record_intent(request, shape) }

        start_transaction!(shape, ach)
        ach
      end

      private

      # The only refusal a caller of this service ever sees, and it covers more
      # than it sounds like: a malformed DAG, a Transaction wider than Go allows,
      # a leg that would move nothing, and — the one that matters here — a
      # withdrawal whose funding leg Go's accept-time pre-flight can already see
      # is short (go/docs/adr/0004). All of them are decided before any event
      # exists, so "before any money moved" is literally true.
      #
      # What it cannot catch is the residual race that pre-flight explicitly
      # does not close: it is a check, not a reservation, and the real dispatch
      # now happens later, in the orchestrator. Such a withdrawal is rolled back
      # without ever reaching the provider, and shows up as a rolled-back
      # Transaction on the row rather than as an error here.
      sig { params(shape: TransactionShape, ach: Models::AchTransaction).void }
      def start_transaction!(shape, ach)
        @go.start_transaction(shape.start_request(transaction_id: ach.id, amount_minor_units: ach.amount_minor_units))
      rescue Refused => e
        raise Refused, "refused before any money moved: #{e.message}"
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
    end
  end
end
