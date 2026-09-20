# frozen_string_literal: true
# typed: strict

require_relative 'repayment_mutation'

module Mutations
  # Takes a payment from the Borrower into the Security. Disbursing it to the
  # holders is a sweep's job, not this one's.
  class RecordSecurityRepayment < RepaymentMutation
    description 'Record a Borrower’s payment of principal and interest into a Security.'

    argument :security_id, ID, required: true
    argument :principal_minor_units, GraphQL::Types::BigInt, required: true
    argument :interest_minor_units, GraphQL::Types::BigInt, required: true

    sig do
      params(security_id: String, principal_minor_units: Integer, interest_minor_units: Integer)
        .returns(T::Hash[Symbol, T.untyped])
    end
    def resolve(security_id:, principal_minor_units:, interest_minor_units:)
      answering do
        repayment = Services::Securities::RecordRepayment.new.call(
          security_id: security_id, principal_minor_units: principal_minor_units,
          interest_minor_units: interest_minor_units
        )
        repayment_payload(repayment.id)
      end
    end
  end
end
