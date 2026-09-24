# frozen_string_literal: true
# typed: strict

require_relative 'names'
require_relative 'world'

module LendingSimulation
  # A loan's life on the platform: offered, drawn once fully subscribed, and
  # paid off with interest, which is disbursed to its holders.
  #
  # Interest is reckoned in simulated days by the domain's own rule
  # (Securities::Interest) and handed to RecordRepayment, which takes the
  # amount from its caller. Nothing in the domain reads the simulated clock.
  class Lending
    sig { params(world: World).void }
    def initialize(world)
      @world = world
    end

    # New loans, a Poisson number of them a day while the issue window lasts.
    sig { void }
    def offer
      return if @world.day >= @world.settings.issue_days

      poisson(@world.profile.market.offerings_per_day).times { offer_one }
    end

    # Every fully subscribed Security's escrow goes to its Borrower, who
    # withdraws it to their bank to spend on the property.
    sig { void }
    def draw
      funded = @world.loans.values.select { |loan| loan.funded? && loan.drawn_day.nil? }
      return if funded.empty?

      @world.platform.draw(funded.map(&:security_id))
      funded.each { |loan| drawn(loan) }
      @world.withdraw(taken_to_bank(funded))
    end

    # Loans due today: the Borrower deposits the sale's proceeds — principal
    # and interest — and repays both, which is disbursed to the holders.
    sig { void }
    def pay_off
      due = @world.loans.values.select { |loan| loan.payoff_day == @world.day }
      interest = due.to_h { |loan| [loan, interest_on(loan)] }
      @world.deposit(sale_proceeds(interest))
      interest.each { |loan, amount| repay(loan, amount) }
    end

    private

    sig { void }
    def offer_one
      random = @world.random
      terms = @world.profile.loan(random)
      borrower_id = @world.borrower_ids.fetch(random.rand(@world.borrower_ids.size))
      security_id = @world.platform.offer(offering_request(terms, borrower_id)).id
      @world.loans[security_id] = Loan.new(security_id: security_id, borrower_id: borrower_id, terms: terms,
                                           remaining_minor_units: terms.principal_minor_units)
    end

    # What each Borrower takes to the bank once drawn: all of it, to spend on
    # the property.
    sig { params(loans: T::Array[Loan]).returns(T::Hash[Integer, Integer]) }
    def taken_to_bank(loans) = by_borrower(loans.to_h { |loan| [loan, loan.terms.principal_minor_units] })

    # What each Borrower deposits at payoff: the sale's proceeds, principal
    # and interest on the loans they pay off.
    sig { params(interest: T::Hash[Loan, Integer]).returns(T::Hash[Integer, Integer]) }
    def sale_proceeds(interest)
      by_borrower(interest.to_h { |loan, amount| [loan, loan.terms.principal_minor_units + amount] })
    end

    # Per-loan amounts, summed per Borrower: one ACH entry each, and none for
    # nothing.
    sig { params(amounts: T::Hash[Loan, Integer]).returns(T::Hash[Integer, Integer]) }
    def by_borrower(amounts)
      owed = Hash.new(0)
      amounts.each { |loan, amount| owed[loan.borrower_id] += amount }
      owed.select { |_id, amount| amount.positive? }
    end

    sig { params(terms: Profile::LoanTerms, borrower_id: Integer).returns(Services::Securities::IssueOffering::Request) }
    def offering_request(terms, borrower_id)
      Services::Securities::IssueOffering::Request.new(
        issuer_entity_id: @world.issuer_id, borrower_entity_id: borrower_id,
        name: Names.property(@world.random, grade: terms.grade, annual_rate_bps: terms.annual_rate_bps),
        principal_minor_units: terms.principal_minor_units, annual_rate_bps: terms.annual_rate_bps,
        term_days: terms.term_days
      )
    end

    sig { params(loan: Loan).void }
    def drawn(loan)
      loan.drawn_day = @world.day
      loan.payoff_day = @world.day + loan.terms.days_to_payoff(minimum: @world.profile.market.minimum_days_drawn)
      @world.book.receive(loan.borrower_id, loan.terms.principal_minor_units)
    end

    # Simple interest over the simulated days drawn, by the domain's rule.
    sig { params(loan: Loan).returns(Integer) }
    def interest_on(loan)
      Services::Securities::Interest.accrued(principal_minor_units: loan.terms.principal_minor_units,
                                             annual_rate_bps: loan.terms.annual_rate_bps,
                                             days: @world.day - T.must(loan.drawn_day))
    end

    sig { params(loan: Loan, interest: Integer).void }
    def repay(loan, interest)
      book = @world.book
      principal = loan.terms.principal_minor_units
      book.spend(loan.borrower_id, principal + interest)
      paid = @world.platform.repay(security_id: loan.security_id, principal: principal, interest: interest,
                                   as_of: @world.today)
      paid.each { |disbursement| book.receive(disbursement.investor_entity_id, paid_out(disbursement)) }
      loan.repaid = true
    end

    sig { params(disbursement: Models::Disbursement).returns(Integer) }
    def paid_out(disbursement) = disbursement.principal_minor_units + disbursement.interest_minor_units

    # Knuth's method: fine for the small means a daily rate gives.
    sig { params(mean: Float).returns(Integer) }
    def poisson(mean)
      limit = Math.exp(-mean)
      count = 0
      product = @world.random.rand
      while product > limit
        count += 1
        product *= @world.random.rand
      end
      count
    end
  end
end
