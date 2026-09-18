# frozen_string_literal: true
# typed: strict

require_relative '../base_service'
require_relative '../det_id'
require_relative 'clearing_shape'
require_relative 'go_gateway'

module Services
  module Ach
    # Originates the clearing Transaction for one ACH deposit.
    #
    # This is a follow-on Transaction (ruby/docs/adr/0001, decision 4; 0003): its
    # ids are derived from the deposit's, so asking again is a no-op in Go, and
    # Ruby keeps no "already cleared" guard of its own. It does not decide
    # whether the deposit is due — ClearDue and ClearingPolicy do.
    class Clear < BaseService
      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      sig { params(ach_transaction_id: String).returns(Models::AchTransaction) }
      def call(ach_transaction_id:)
        ach = find_deposit!(ach_transaction_id)
        transaction_id = DetId.for("#{ach.id}:clearing")
        shape = shape_for(ach)
        perform { ach.update(clearing_transaction_id: transaction_id, clearing_transfer_id: shape.transfer_id) }

        @go.start_transaction(shape.start_request(transaction_id: transaction_id,
                                                  amount_minor_units: ach.amount_minor_units))
        ach
      end

      private

      sig { params(id: String).returns(Models::AchTransaction) }
      def find_deposit!(id)
        ach = Models::AchTransaction[id] || raise(NotFound, "no ACH transaction #{id}")
        return ach if ach.direction == Types::Enums::AchDirection::Deposit.serialize

        raise NotClearable, "#{ach.id} is a #{ach.direction}; only deposits are cleared"
      end

      sig { params(ach: Models::AchTransaction).returns(ClearingShape) }
      def shape_for(ach)
        ClearingShape.for(accounts: Models::Account.where(entity_id: ach.entity_id).all,
                          transfer_id: DetId.for("#{ach.id}:clearing:transfer"))
      end
    end
  end
end
