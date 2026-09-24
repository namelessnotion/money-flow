# frozen_string_literal: true

require 'spec_helper'
require_relative '../../../lib/lending_simulation'

RSpec.describe LendingSimulation::Book do
  subject(:book) { described_class.new }

  it 'starts every entity with nothing' do
    expect(book.cleared(7)).to eq(0)
    expect(book.withdrawable(7)).to eq(0)
  end

  it 'makes a deposit both spendable and withdrawable' do
    book.deposit(7, 5_000)

    expect(book.cleared(7)).to eq(5_000)
    expect(book.withdrawable(7)).to eq(5_000)
  end

  it 'spends and receives on the platform on both sides' do
    # Every Securities money leg has a cash leg beside it (ruby/docs/adr/0009).
    book.deposit(7, 5_000)
    book.spend(7, 1_200)
    book.receive(7, 300)

    expect([book.cleared(7), book.cash(7)]).to eq([4_100, 4_100])
  end

  it 'lets money received on the platform leave again' do
    # A Borrower's Draw, an Investor's interest.
    book.receive(7, 500)

    expect(book.withdrawable(7)).to eq(500)
  end

  it 'takes a withdrawal out of both' do
    book.deposit(7, 1_000)
    book.withdraw(7, 400)

    expect([book.cleared(7), book.withdrawable(7)]).to eq([600, 600])
  end

  it 'refuses to spend money the entity does not have' do
    # The simulation mirrors the ledger so it never asks for something the
    # ledger would refuse; overdrawing the mirror is its bug.
    book.deposit(7, 100)

    expect { book.spend(7, 101) }.to raise_error(LendingSimulation::Book::Overdrawn, /7/)
    expect { book.withdraw(7, 101) }.to raise_error(LendingSimulation::Book::Overdrawn, /7/)
  end

  it 'answers every cleared balance at once, for planning' do
    book.deposit(1, 10)
    book.deposit(2, 20)

    expect(book.cleared_balances([1, 2, 3])).to eq({ 1 => 10, 2 => 20, 3 => 0 })
  end
end
