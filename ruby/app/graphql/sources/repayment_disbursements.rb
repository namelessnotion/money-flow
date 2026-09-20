# frozen_string_literal: true
# typed: strict

module Sources
  # Batches Disbursement lookups by Repayment, so a page of Repayments costs
  # one query rather than one per row.
  #
  # Loads them through Types::Disbursement's own dataset, so each comes back
  # with its projected state joined in rather than fetched again per row.
  class RepaymentDisbursements < GraphQL::Dataloader::Source
    sig { params(repayment_ids: T::Array[String]).returns(T::Array[T::Array[Models::Disbursement]]) }
    def fetch(repayment_ids)
      rows = Types::Disbursement.dataset
                                .where(Sequel[:disbursements][:repayment_id] => repayment_ids)
                                .order(Sequel[:disbursements][:investor_entity_id])
                                .all
      by_repayment = rows.group_by(&:repayment_id)
      repayment_ids.map { |id| by_repayment.fetch(id, []) }
    end
  end
end
