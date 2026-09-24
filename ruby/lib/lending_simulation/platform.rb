# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'await'

module LendingSimulation
  # The platform as the simulation uses it: the same services GraphQL calls,
  # each followed by waiting for the read model to see what became of it. The
  # only place the simulation touches the stack, so the Market reads as the
  # market's story rather than as plumbing.
  #
  # Every method takes a batch — a day's deposits, a day's Subscriptions — and
  # starts them all before awaiting any, so a busy day costs one round of
  # polling rather than one per Transaction.
  class Platform
    Ach = Services::Ach
    Securities = Services::Securities
    AchDirection = Types::Enums::AchDirection
    TransferState = Types::Enums::TransferState

    sig { params(await: Await).void }
    def initialize(await:)
      @await = await
      # The sweeps log every run at info; the simulation calls them per entry.
      quiet = ::Logger.new($stdout, level: ::Logger::WARN)
      @submit = T.let(Ach::SubmitDue.new(logger: quiet), Ach::SubmitDue)
      @disburse = T.let(Securities::DisburseDue.new(logger: quiet), Securities::DisburseDue)
    end

    sig { params(name: String, role: Types::Enums::EntityRole).returns(Integer) }
    def onboard(name, role)
      Services::OnboardEntity.new.call(request: OnboardEntityRequest.new(name: name, role: role)).entity_id
    end

    # Deposits that have cleared: initiated, submitted, settled by the
    # simulated provider, completed, and cleared at once rather than after the
    # return window (Ach::ClearNow) — the money is spendable when this returns.
    sig { params(amounts: T::Hash[Integer, Integer]).void }
    def deposit(amounts)
      achs = settled(amounts, AchDirection::Deposit)
      cleared = achs.map { |ach| Ach::ClearNow.new.call(ach_transaction_id: ach.id).clearing_transaction_id }
      @await.completed!(cleared.compact)
    end

    # Withdrawals that have left: funded from cleared cash, submitted, settled.
    sig { params(amounts: T::Hash[Integer, Integer]).void }
    def withdraw(amounts)
      settled(amounts, AchDirection::Withdrawal)
    end

    # An open offering, its Supply minted and ready to sell.
    sig { params(request: Securities::IssueOffering::Request).returns(Models::Security) }
    def offer(request)
      security = Securities::IssueOffering.new.call(request: request)
      @await.completed!([T.must(security.offering_transaction_id)])
      security
    end

    # Subscriptions, each completed or not: a caller must credit back the
    # money of any that did not.
    sig { params(purchases: T::Array[AutoInvest::PlannedPurchase]).returns(T::Hash[AutoInvest::PlannedPurchase, T::Boolean]) }
    def purchase(purchases)
      started = purchases.to_h do |planned|
        subscription = Securities::Purchase.new.call(security_id: planned.security_id,
                                                     investor_entity_id: planned.investor_entity_id,
                                                     amount_minor_units: planned.amount_minor_units)
        [planned, subscription.id]
      end
      outcome = @await.settled(started.values)
      started.transform_values { |id| outcome.fetch(id) == Types::Enums::TransactionState::Completed.serialize }
    end

    sig { params(security_ids: T::Array[String]).void }
    def draw(security_ids)
      drawn = security_ids.map { |id| T.must(Securities::Draw.new.call(security_id: id).draw_transaction_id) }
      @await.completed!(drawn)
    end

    # A Repayment, completed, then disbursed to every holder: returns what
    # each Investor was paid.
    sig do
      params(security_id: String, principal: Integer, interest: Integer, as_of: Date)
        .returns(T::Array[Models::Disbursement])
    end
    def repay(security_id:, principal:, interest:, as_of:)
      repayment = Securities::RecordRepayment.new.call(security_id: security_id, principal_minor_units: principal,
                                                       interest_minor_units: interest, as_of: as_of)
      @await.completed!([repayment.id])
      result = @disburse.call(repayment_id: repayment.id)
      raise Await::Failed, "disbursement not sent: #{result.failed}" unless result.failed.empty?

      @await.completed!(result.disbursed)
      Models::Disbursement.where(id: result.disbursed).all
    end

    private

    # An ACH Transaction per entity, taken through submission and settlement
    # to completion.
    sig { params(amounts: T::Hash[Integer, Integer], direction: AchDirection).returns(T::Array[Models::AchTransaction]) }
    def settled(amounts, direction)
      achs = amounts.map do |entity_id, amount|
        Ach::Initiate.new.call(request: InitiateAchRequest.new(entity_id: entity_id, direction: direction,
                                                               amount_minor_units: amount))
      end
      submit_and_settle(achs)
      @await.completed!(achs.map(&:id))
      achs
    end

    # The provider's part: take each entry once its real leg is staged, and
    # report it posted once it is pending.
    sig { params(achs: T::Array[Models::AchTransaction]).void }
    def submit_and_settle(achs)
      real_legs = achs.map(&:real_transfer_id)
      @await.transfers!(real_legs, state: TransferState::Staged)
      achs.each { |ach| @submit.call(ach_transaction_id: ach.id) }
      @await.transfers!(real_legs, state: TransferState::Pending)
      achs.each { |ach| Ach::Settle.new.call(ach_transaction_id: ach.id) }
    end
  end
end
