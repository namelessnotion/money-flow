import type { AchTransactionDetail, AchTransactionSummary } from '../graphql/ach'

export function achSummary(overrides: Partial<AchTransactionSummary> = {}) {
  return {
    __typename: 'AchTransaction',
    id: '0199a0b0-0000-7000-8000-000000000001',
    direction: 'DEPOSIT',
    amountMinorUnits: '12500',
    currency: 'USD',
    state: 'STARTED',
    createdAt: '2026-09-18T14:00:00Z',
    ...overrides,
  }
}

export function achDetail(overrides: Partial<AchTransactionDetail> = {}) {
  return {
    ...achSummary(),
    providerReference: 'fake-1',
    reason: null,
    realLegState: 'PENDING',
    clearingState: null,
    clearingDueOn: null,
    steps: [
      { __typename: 'AchStep', name: 'INITIATION', status: 'DONE' },
      { __typename: 'AchStep', name: 'SUBMISSION', status: 'DONE' },
      { __typename: 'AchStep', name: 'SETTLEMENT', status: 'WAITING' },
      { __typename: 'AchStep', name: 'COMPLETION', status: 'WAITING' },
      { __typename: 'AchStep', name: 'CLEARING', status: 'WAITING' },
    ],
    ...overrides,
  }
}
