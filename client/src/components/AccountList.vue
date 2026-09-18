<script setup lang="ts">
import type { AccountBalance, AccountNode } from '../graphql/entity'
import { formatMinorUnits } from '../lib/money'

defineProps<{ accounts: AccountNode[] }>()

// An Account with no ledger activity yet has no balances; it holds nothing.
const EMPTY: AccountBalance[] = [
  { currency: 'USD', postedMinorUnits: '0', pendingOutgoingMinorUnits: '0', pendingIncomingMinorUnits: '0' },
]

function shown(balances: AccountBalance[]): AccountBalance[] {
  return balances.length > 0 ? balances : EMPTY
}

function isPending(minorUnits: string): boolean {
  return BigInt(minorUnits) !== 0n
}
</script>

<template>
  <section class="mb-8">
    <h2 class="mb-3 text-lg font-bold text-slate-900">Accounts</h2>
    <p v-if="accounts.length === 0" class="rounded-lg bg-slate-100 p-4 text-slate-600">This entity has no accounts.</p>
    <ul v-else class="divide-y divide-slate-200 overflow-hidden rounded-lg border border-slate-200 bg-white shadow-sm">
      <li v-for="account in accounts" :key="account.id" class="flex items-center justify-between gap-4 px-4 py-3">
        <div class="min-w-0">
          <p class="truncate font-semibold text-slate-900">
            {{ account.name }}
            <span class="ml-2 rounded-full bg-slate-100 px-2.5 py-0.5 text-xs font-bold text-slate-600">
              {{ account.type }}
            </span>
          </p>
          <p class="truncate text-sm text-slate-500">{{ account.walletUuid }}</p>
        </div>
        <div class="shrink-0 text-right">
          <div v-for="balance in shown(account.balances)" :key="balance.currency">
            <p
              data-balance="posted"
              class="font-semibold tabular-nums"
              :class="balance.postedMinorUnits.startsWith('-') ? 'text-red-700' : 'text-slate-900'"
            >
              {{ formatMinorUnits(balance.postedMinorUnits, balance.currency) }}
            </p>
            <p
              v-if="isPending(balance.pendingOutgoingMinorUnits)"
              data-balance="pending-out"
              class="text-xs text-amber-700 tabular-nums"
            >
              −{{ formatMinorUnits(balance.pendingOutgoingMinorUnits, balance.currency) }} pending out
            </p>
            <p
              v-if="isPending(balance.pendingIncomingMinorUnits)"
              data-balance="pending-in"
              class="text-xs text-emerald-700 tabular-nums"
            >
              +{{ formatMinorUnits(balance.pendingIncomingMinorUnits, balance.currency) }} pending in
            </p>
          </div>
        </div>
      </li>
    </ul>
  </section>
</template>
