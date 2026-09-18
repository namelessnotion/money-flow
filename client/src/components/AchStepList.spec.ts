import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import AchStepList from './AchStepList.vue'
import type { AchStep } from '../graphql/ach'

function statuses(wrapper: ReturnType<typeof mount>) {
  return wrapper.findAll('li').map((li) => [li.attributes('data-step'), li.attributes('data-status')])
}

describe('AchStepList', () => {
  it('lists every step with where it stands', () => {
    const steps: AchStep[] = [
      { name: 'INITIATION', status: 'DONE' },
      { name: 'SUBMISSION', status: 'DONE' },
      { name: 'SETTLEMENT', status: 'WAITING' },
      { name: 'COMPLETION', status: 'WAITING' },
    ]
    const wrapper = mount(AchStepList, { props: { steps, reason: null, clearingDueOn: null } })

    expect(statuses(wrapper)).toEqual([
      ['INITIATION', 'DONE'],
      ['SUBMISSION', 'DONE'],
      ['SETTLEMENT', 'WAITING'],
      ['COMPLETION', 'WAITING'],
    ])
    expect(wrapper.text()).toContain('Settled')
  })

  it('shows the reason against the step that failed', () => {
    const steps: AchStep[] = [
      { name: 'SETTLEMENT', status: 'FAILED' },
      { name: 'COMPLETION', status: 'SKIPPED' },
    ]
    const wrapper = mount(AchStepList, { props: { steps, reason: 'R01 insufficient funds', clearingDueOn: null } })

    expect(wrapper.get('[data-step=SETTLEMENT]').text()).toContain('R01 insufficient funds')
    expect(wrapper.get('[data-step=COMPLETION]').text()).not.toContain('R01')
  })

  it('names the funding and rollback steps a failed withdrawal goes through', () => {
    const steps: AchStep[] = [
      { name: 'INITIATION', status: 'DONE' },
      { name: 'FUNDING', status: 'FAILED' },
      { name: 'SUBMISSION', status: 'SKIPPED' },
      { name: 'ROLLBACK', status: 'WAITING' },
    ]
    const wrapper = mount(AchStepList, { props: { steps, reason: 'insufficient cleared cash', clearingDueOn: null } })

    expect(wrapper.get('[data-step=FUNDING]').text()).toContain('Funded')
    expect(wrapper.get('[data-step=FUNDING]').text()).toContain('insufficient cleared cash')
    expect(wrapper.get('[data-step=ROLLBACK]').text()).toContain('Rolled back')
    expect(wrapper.get('[data-step=ROLLBACK]').text()).toContain('Waiting')
  })

  it('shows when a waiting clearing is due', () => {
    const wrapper = mount(AchStepList, {
      props: { steps: [{ name: 'CLEARING', status: 'WAITING' }], reason: null, clearingDueOn: '2026-09-17' },
    })

    expect(wrapper.get('[data-step=CLEARING]').text()).toContain('Due Sep 17, 2026')
  })
})
