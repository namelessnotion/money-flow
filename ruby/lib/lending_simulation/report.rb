# frozen_string_literal: true
# typed: strict

require 'logger'
require_relative 'money'

module LendingSimulation
  # What a run did, read back through the money flow rather than from the
  # simulation's own bookkeeping — so the report is also a check that the read
  # model tells the same story the simulation meant to write.
  class Report
    Kind = Services::MoneyFlow::Movement::Kind

    # A conservation rule the run broke.
    class Discrepancy < T::Struct
      const :rule, String
      const :detail, String
    end

    # The account types the Book mirrors.
    MIRRORED = T.let(%w[cleared_cash cash].freeze, T::Array[String])

    sig { params(tag: String, book: Book, logger: ::Logger).void }
    def initialize(tag:, book:, logger:)
      @tag = tag
      @book = book
      @logger = logger
    end

    # Logs the totals and the checks; true when every check held.
    sig { params(entity_ids: T::Array[Integer]).returns(T::Boolean) }
    def call(entity_ids)
      flow = Services::MoneyFlow.of(name_prefix: @tag)
      log_totals(flow)
      discrepancies = conservation(flow) + unreconciled(entity_ids)
      discrepancies.each { |d| @logger.error("#{d.rule}: #{d.detail}") }
      @logger.info(discrepancies.empty? ? 'every check held' : "#{discrepancies.size} check(s) failed")
      discrepancies.empty?
    end

    private

    sig { params(flow: Services::MoneyFlow::Flow).void }
    def log_totals(flow)
      Kind.each_value do |kind|
        movements = flow.movements.select { |m| m.kind == kind }
        @logger.info(format('%<kind>-24s %<count>6d  %<total>16s', kind: kind.serialize, count: movements.size,
                                                                   total: Money.format(total(movements))))
      end
      @logger.info("money flow: moneyFlow(namePrefix: \"#{@tag}\") — #{flow.parties.size} parties, " \
                   "#{flow.movements.size} movements")
    end

    # Every Repayment reached the holders, and every Draw was paid for by the
    # Subscriptions into its Security.
    sig { params(flow: Services::MoneyFlow::Flow).returns(T::Array[Discrepancy]) }
    def conservation(flow)
      by_kind = flow.movements.group_by(&:kind)
      repaid = total(by_kind.fetch(Kind::Repayment, []))
      disbursed = total(by_kind.fetch(Kind::DisbursementPrincipal, []) + by_kind.fetch(Kind::DisbursementInterest, []))
      found = []
      found << Discrepancy.new(rule: 'repaid = disbursed', detail: "#{repaid} vs #{disbursed}") if repaid != disbursed
      found.concat(underfunded_draws(by_kind))
    end

    sig { params(by_kind: T::Hash[Kind, T::Array[Services::MoneyFlow::Movement]]).returns(T::Array[Discrepancy]) }
    def underfunded_draws(by_kind)
      subscribed = by_kind.fetch(Kind::Subscription, []).group_by(&:target).transform_values { |ms| total(ms) }
      by_kind.fetch(Kind::Draw, []).filter_map do |draw|
        next if subscribed.fetch(draw.source, 0) == draw.amount_minor_units

        Discrepancy.new(rule: 'draw = subscriptions',
                        detail: "#{draw.source} drew #{draw.amount_minor_units}, sold #{subscribed[draw.source]}")
      end
    end

    # Every entity's cleared cash and cash on the ledger, as projected, are
    # what the Book says they should be. Balances project a little behind the
    # Transactions, so this gives them a moment.
    sig { params(entity_ids: T::Array[Integer], attempts: Integer).returns(T::Array[Discrepancy]) }
    def unreconciled(entity_ids, attempts: 50)
      wallets = wallets_of(entity_ids)
      found = T.let([], T::Array[Discrepancy])
      attempts.times do
        found = mismatched(wallets)
        break if found.empty?

        sleep(0.2)
      end
      found
    end

    # [entity id, account type] => wallet uuid.
    sig { params(entity_ids: T::Array[Integer]).returns(T::Hash[[Integer, String], String]) }
    def wallets_of(entity_ids)
      DB[:accounts].where(entity_id: entity_ids, type: MIRRORED, security_id: nil)
                   .select_map(%i[entity_id type wallet_uuid])
                   .to_h { |id, type, uuid| [[Integer(id), String(type)], String(uuid)] }
    end

    sig { params(wallets: T::Hash[[Integer, String], String]).returns(T::Array[Discrepancy]) }
    def mismatched(wallets)
      posted = Models::TokenBalanceProjection.totals_for(wallets.values)
      wallets.filter_map do |(id, type), uuid|
        mirrored = type == 'cash' ? @book.cash(id) : @book.cleared(id)
        ledger = posted.fetch(uuid, []).sum(&:posted)
        next if mirrored == ledger

        Discrepancy.new(rule: "#{type} = book", detail: "entity #{id}: book #{mirrored}, ledger #{ledger}")
      end
    end

    sig { params(movements: T::Array[Services::MoneyFlow::Movement]).returns(Integer) }
    def total(movements) = movements.sum(&:amount_minor_units)
  end
end
