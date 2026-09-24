import { createRouter, createWebHistory, type RouterHistory } from 'vue-router'
import EntitiesView from './views/EntitiesView.vue'
import EntityView from './views/EntityView.vue'
import AchTransactionView from './views/AchTransactionView.vue'
import MoneyFlowView from './views/MoneyFlowView.vue'

export const routes = [
  { path: '/', name: 'entities', component: EntitiesView },
  { path: '/entities/:entityId', name: 'entity', component: EntityView, props: true },
  {
    path: '/entities/:entityId/ach-transactions/:id',
    name: 'ach-transaction',
    component: AchTransactionView,
    props: true,
  },
  // A graph needs the width the entity pages do not.
  { path: '/money-flow', name: 'money-flow', component: MoneyFlowView, meta: { wide: true } },
]

export function makeRouter(history: RouterHistory = createWebHistory()) {
  return createRouter({ history, routes })
}
