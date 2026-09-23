# frozen_string_literal: true
# typed: strict

require_relative '../base_service'
require_relative '../det_id'
require_relative 'draw_shape'
require_relative 'errors'
require_relative 'go_gateway'
require_relative 'stage'

module Services
  module Securities
    # Moves a fully-subscribed Security's escrowed money to its Borrower.
    #
    # Operator-initiated rather than a reaction to the last Subscription
    # completing, and deliberately so: it is what keeps this capability's
    # follow-on chain one step long (ruby/docs/adr/0007). Nothing in the
    # platform watches a Subscription.
    #
    # 1. Requires the Security to be fully subscribed and not already drawn.
    # 2. Records the ids it is about to send, derived from the Security, so
    #    asking twice converges on Go rather than drawing twice.
    # 3. Asks Go to accept it. The leg is a root and mints nothing, so a short
    #    escrow is refused at accept time, before anything is written — which
    #    is the answer an operator who pressed the button actually needs.
    #
    # It does not wait to see the money move. Go accepts the Transaction and
    # returns; running it belongs to the orchestrator, and the outcome reaches
    # Ruby through the projection like every other lifecycle fact
    # (go/docs/adr/0006). Services::Securities::Stage is where it shows up.
    #
    # Getting that money out to a real bank is an ordinary ACH withdrawal from
    # the Borrower's own cleared cash. The Draw does not cross the boundary.
    class Draw < BaseService
      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      sig { params(security_id: String).returns(Models::Security) }
      def call(security_id:)
        security = Models::Security[security_id] || raise(NotFound, "no security #{security_id}")
        ensure_drawable!(security)

        perform { record_ids(security) }
        start_transaction!(security)
        security
      end

      private

      sig { params(security: Models::Security).void }
      def ensure_drawable!(security)
        if security.draw_transaction_id && Models::TransactionProjection[security.draw_transaction_id]
          raise NotDrawable, "#{security.id} has already been drawn"
        end

        stage = Stage.current(Stage.snapshot_of(security))
        return if stage == Stage::Name::Funded

        raise NotDrawable, "#{security.id} is #{stage.serialize}; only a fully subscribed Security draws"
      end

      # Derived from the Security, so these are a record of what was sent
      # rather than an "already drawn" flag — Go is that.
      sig { params(security: Models::Security).void }
      def record_ids(security)
        security.update(
          draw_transaction_id: DetId.for("security:#{security.id}:draw"),
          draw_transfer_id: DetId.for("security:#{security.id}:draw:transfer")
        )
      end

      sig { params(security: Models::Security).void }
      def start_transaction!(security)
        accounts = Models::Account.where(entity_id: [security.issuer_entity_id, security.borrower_entity_id]).all
        shape = DrawShape.for(security: security, accounts: accounts,
                              transfer_id: required(security, :draw_transfer_id))
        @go.start_transaction(
          shape.start_request(transaction_id: required(security, :draw_transaction_id),
                              amount_minor_units: security.principal_minor_units)
        )
      rescue Refused => e
        raise Refused, "refused before any money moved: #{e.message}"
      end

      # The draw ids are nullable on the row until record_ids writes them, but
      # nothing may be sent to Go without them.
      sig { params(security: Models::Security, column: Symbol).returns(String) }
      def required(security, column)
        security.public_send(column) || raise(NotFound, "security #{security.id} has no #{column}")
      end
    end
  end
end
