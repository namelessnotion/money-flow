# frozen_string_literal: true
# typed: strict

require_relative 'objects/base_object'
require_relative '../mutations/onboard_entity'
require_relative '../mutations/initiate_ach'
require_relative '../mutations/settle_ach'
require_relative '../mutations/return_ach'

module Types
  # Root Mutation type
  class MutationType < BaseObject
    field :onboard_entity, mutation: Mutations::OnboardEntity
    field :initiate_ach_deposit, mutation: Mutations::InitiateAchDeposit
    field :initiate_ach_withdrawal, mutation: Mutations::InitiateAchWithdrawal
    field :settle_ach, mutation: Mutations::SettleAch
    field :return_ach, mutation: Mutations::ReturnAch
  end
end
