# frozen_string_literal: true
# typed: strict

require 'securerandom'
require_relative '../base_service'
require_relative 'errors'
require_relative 'go_gateway'
require_relative 'positions'
require_relative 'repayment_shape'
require_relative 'schedule'
require_relative 'stage'

module Services
  module Securities
    # Takes a payment from the Borrower into the Security.
    #
    # 1. Requires the Security to have been drawn — there is nothing to repay
    #    before the Borrower has the money — and the principal not to exceed
    #    what is still owed.
    # 2. Records the payment and the ids it is about to send, before calling
    #    Go, so a retry resends the same ids and converges.
    # 3. Asks Go to accept it. The leg is a root and mints nothing, so an
    #    underfunded Borrower is refused at accept time, before anything is
    #    written — the one refusal a caller learns here.
    #
    # It does not wait for the money to arrive. Go accepts and returns; the
    # orchestrator runs it, and the outcome reaches Ruby through the projection
    # (go/docs/adr/0006). That is also what the disbursement sweep waits on: it
    # only acts on a Repayment the read model has seen complete.
    #
    # Disbursing it to the holders is not this service's job: that is a sweep,
    # one Transaction per holder (ruby/docs/adr/0007). The Borrower gets the
    # cleared cash to pay from the ordinary way — an ACH deposit that cleared.
    class RecordRepayment < BaseService
      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      sig do
        params(security_id: String, principal_minor_units: Integer, interest_minor_units: Integer,
               as_of: Date).returns(Models::Repayment)
      end
      def call(security_id:, principal_minor_units:, interest_minor_units:, as_of: Date.today)
        security = Models::Security[security_id] || raise(NotFound, "no security #{security_id}")
        validate!(security, principal_minor_units, interest_minor_units)

        repayment = perform { record(security, principal_minor_units, interest_minor_units, as_of) }

        start_transaction!(security, repayment)
        repayment
      end

      private

      sig { params(security: Models::Security, principal: Integer, interest: Integer).void }
      def validate!(security, principal, interest)
        raise InvalidAmount, 'neither part may be negative' if principal.negative? || interest.negative?
        # Go refuses a zero-amount Transfer, so there would be no leg to send.
        raise InvalidAmount, 'a repayment of nothing has no leg to send' unless (principal + interest).positive?

        ensure_drawn!(security)

        outstanding = Positions.outstanding_principal(security)
        return if principal <= outstanding

        raise NotRepayable, "#{security.id} has #{outstanding} principal outstanding, not #{principal}"
      end

      sig { params(security: Models::Security).void }
      def ensure_drawn!(security)
        return if Schedule.drawn_on(security)

        stage = Stage.current(Stage.snapshot_of(security))
        raise NotRepayable, "#{security.id} is #{stage.serialize}; nothing is owed until it is drawn"
      end

      sig do
        params(security: Models::Security, principal: Integer, interest: Integer, as_of: Date)
          .returns(Models::Repayment)
      end
      def record(security, principal, interest, as_of)
        Models::Repayment.create(
          id: SecureRandom.uuid_v7,
          security_id: security.id,
          principal_minor_units: principal,
          interest_minor_units: interest,
          as_of: as_of,
          currency: CURRENCY,
          collection_transfer_id: SecureRandom.uuid_v7
        )
      end

      sig { params(security: Models::Security, repayment: Models::Repayment).void }
      def start_transaction!(security, repayment)
        accounts = Models::Account.where(entity_id: [security.issuer_entity_id, security.borrower_entity_id]).all
        shape = RepaymentShape.for(security: security, accounts: accounts,
                                   transfer_id: repayment.collection_transfer_id)
        @go.start_transaction(
          shape.start_request(transaction_id: repayment.id, amount_minor_units: total(repayment))
        )
      rescue Refused => e
        raise Refused, "refused before any money moved: #{e.message}"
      end

      sig { params(repayment: Models::Repayment).returns(Integer) }
      def total(repayment)
        repayment.principal_minor_units + repayment.interest_minor_units
      end
    end
  end
end
