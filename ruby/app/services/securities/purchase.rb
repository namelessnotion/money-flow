# frozen_string_literal: true
# typed: strict

require 'securerandom'
require_relative '../base_service'
require_relative 'errors'
require_relative 'go_gateway'
require_relative 'positions'
require_relative 'purchase_shape'
require_relative 'stage'

module Services
  module Securities
    # Buys a fraction of a Security for an Investor.
    #
    # 1. Records the intent — the Security, the Investor, the amount and every
    #    id — before calling anything, so a retry or a re-run after a crash
    #    resends the same ids and converges on Go's idempotency rather than
    #    buying twice.
    # 2. Asks Go to run the Transaction. The claim leg gates, so an
    #    oversubscription is normally refused here, before any event is written
    #    and before any of the Investor's money moves.
    # 3. Asks Go how the Transaction actually stands. Nothing here stages, so
    #    the whole DAG runs to completion inside step 2's call — but the money
    #    leg sits behind a dependency edge and so gets no accept-time
    #    pre-flight at all. This is the only thing that ever learns its real
    #    outcome, and step 2's answer can never be the last word for it.
    # 4. Returns the Subscription. Lifecycle state is not recorded here: it
    #    arrives through the projection.
    class Purchase < BaseService
      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      sig do
        params(security_id: String, investor_entity_id: Integer, amount_minor_units: Integer)
          .returns(Models::Subscription)
      end
      def call(security_id:, investor_entity_id:, amount_minor_units:)
        security = find!(security_id)
        investor = investor!(investor_entity_id)
        check_offering!(security, amount_minor_units)

        shape = plan(security, investor)
        subscription = perform { record_intent(security, investor, amount_minor_units, shape) }

        start_transaction!(shape, subscription)
        require_settled!(subscription)
        subscription
      end

      private

      sig { params(security_id: String).returns(Models::Security) }
      def find!(security_id)
        Models::Security[security_id] || raise(NotFound, "no security #{security_id}")
      end

      sig { params(investor_entity_id: Integer).returns(Models::Entity) }
      def investor!(investor_entity_id)
        entity = Models::Entity[investor_entity_id] || raise(NotFound, "no entity #{investor_entity_id}")
        role = Types::Enums::EntityRole::Investor
        return entity if entity.role == role.serialize

        raise WrongRole, "entity #{investor_entity_id} is a #{entity.role}, not an investor"
      end

      # Advisory, all of it. The read model lags the ledger, so a purchase this
      # lets through can still be refused by the Supply wallet, and one it
      # turns away might have squeaked through. It is here for the error
      # message, not for the guarantee: the guarantee is that security_supply
      # permits no debit past its credits.
      sig { params(security: Models::Security, amount_minor_units: Integer).void }
      def check_offering!(security, amount_minor_units)
        raise InvalidAmount, 'amount must be positive' unless amount_minor_units.positive?

        snapshot = Stage.snapshot_of(security)
        raise Oversubscribed, not_open(security, snapshot) unless Stage.open_for_subscription?(snapshot)

        remaining = security.principal_minor_units - snapshot.subscribed_minor_units
        return if amount_minor_units <= remaining

        raise Oversubscribed, "#{security.id} has #{remaining} left, as the read model last saw it"
      end

      sig { params(security: Models::Security, snapshot: Stage::Snapshot).returns(String) }
      def not_open(security, snapshot)
        "#{security.id} is not open for subscription (#{Stage.current(snapshot).serialize}, " \
          "#{snapshot.subscribed_minor_units} of #{snapshot.principal_minor_units} sold)"
      end

      sig { params(security: Models::Security, investor: Models::Entity).returns(PurchaseShape) }
      def plan(security, investor)
        accounts = Models::Account.where(entity_id: [security.issuer_entity_id, investor.id]).all
        PurchaseShape.for(security: security, investor_entity_id: investor.id, accounts: accounts,
                          claim_transfer_id: SecureRandom.uuid_v7, money_transfer_id: SecureRandom.uuid_v7)
      end

      sig do
        params(security: Models::Security, investor: Models::Entity, amount_minor_units: Integer,
               shape: PurchaseShape).returns(Models::Subscription)
      end
      def record_intent(security, investor, amount_minor_units, shape)
        Models::Subscription.create(
          id: SecureRandom.uuid_v7,
          security_id: security.id,
          investor_entity_id: investor.id,
          amount_minor_units: amount_minor_units,
          currency: CURRENCY,
          claim_transfer_id: shape.claim_transfer_id,
          money_transfer_id: shape.money_transfer_id
        )
      end

      # An oversubscription is caught by Go's own accept/reject decision, before
      # any event exists. Re-wrapped with the same framing require_settled!
      # uses, so the message a caller sees does not depend on which check
      # caught it.
      sig { params(shape: PurchaseShape, subscription: Models::Subscription).void }
      def start_transaction!(shape, subscription)
        @go.start_transaction(
          shape.start_request(transaction_id: subscription.id,
                              amount_minor_units: subscription.amount_minor_units)
        )
      rescue Refused => e
        raise Refused, "refused before any money moved: #{e.message}"
      end

      # The money leg is gated and so is never pre-flighted: an Investor short
      # of cleared cash gets a Transaction that initializes and then rolls back,
      # with Go reversing the already-committed claim leg. Nothing is left
      # half-done, but the caller has to be told, and only this can tell them.
      sig { params(subscription: Models::Subscription).void }
      def require_settled!(subscription)
        outcome = @go.resume(subscription.id)
        return if outcome.completed? || outcome.started?

        raise Refused, "refused: #{outcome.reason.empty? ? outcome.state : outcome.reason}"
      end
    end
  end
end
