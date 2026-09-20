# frozen_string_literal: true
# typed: strict

require_relative 'objects/base_object'
require_relative '../mutations/onboard_entity'
require_relative '../mutations/initiate_ach'
require_relative '../mutations/settle_ach'
require_relative '../mutations/return_ach'
require_relative '../mutations/clear_ach'
require_relative '../mutations/issue_security'
require_relative '../mutations/purchase_security'
require_relative '../mutations/draw_security'
require_relative '../mutations/record_security_repayment'
require_relative '../mutations/disburse_repayment_now'

module Types
  # Root Mutation type
  class MutationType < BaseObject
    field :onboard_entity, mutation: Mutations::OnboardEntity
    field :initiate_ach_deposit, mutation: Mutations::InitiateAchDeposit
    field :initiate_ach_withdrawal, mutation: Mutations::InitiateAchWithdrawal
    field :settle_ach, mutation: Mutations::SettleAch
    field :return_ach, mutation: Mutations::ReturnAch
    field :clear_ach, mutation: Mutations::ClearAch

    field :issue_security, mutation: Mutations::IssueSecurity
    field :purchase_security, mutation: Mutations::PurchaseSecurity
    field :draw_security, mutation: Mutations::DrawSecurity
    field :record_security_repayment, mutation: Mutations::RecordSecurityRepayment
    field :disburse_repayment_now, mutation: Mutations::DisburseRepaymentNow
  end
end
