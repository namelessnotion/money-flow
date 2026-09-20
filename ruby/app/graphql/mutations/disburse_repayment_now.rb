# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'repayment_mutation'

module Mutations
  # Stands in for the scheduled disbursement sweep in demonstrations, so a
  # completed Repayment need not wait for the next run.
  class DisburseRepaymentNow < RepaymentMutation
    description 'Disburse a completed Repayment to its holders now, without waiting for the sweep ' \
                '(for demonstrations).'

    argument :repayment_id, ID, required: true

    sig { params(repayment_id: String).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(repayment_id:)
      answering do
        raise Services::Securities::NotFound, "no repayment #{repayment_id}" if Types::Repayment.find(repayment_id).nil?

        sweep(repayment_id)
        repayment_payload(repayment_id)
      end
    end

    private

    # The failures are already logged and visible in each Disbursement's
    # projection, so a partial run answers with the Repayment rather than
    # raising: the holders who were paid were paid.
    sig { params(repayment_id: String).void }
    def sweep(repayment_id)
      Services::Securities::DisburseDue
        .new(logger: Logger.new($stdout, progname: self.class.name))
        .call(repayment_id: repayment_id)
    end
  end
end
