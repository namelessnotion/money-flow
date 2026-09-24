<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import cytoscape, { type Core, type ElementDefinition, type StylesheetJson } from 'cytoscape'
import { FLOW_COLOURS, flowOf, type GraphEdge, type GraphNode, type Position } from '../lib/moneyFlowGraph'

// Draws the money flow: a node per Party at a fixed place, an edge per
// (source, target, kind) as wide as the money along it. Elements are diffed
// in place on every change rather than rebuilt, so a replay frame costs only
// what changed and nothing moves.

const props = defineProps<{
  nodes: GraphNode[]
  edges: GraphEdge[]
  positions: Map<string, Position>
  // The largest edge in the whole run, so widths mean the same thing at every
  // point of the replay instead of growing and shrinking with the cutoff.
  largestMinorUnits: bigint
  selected: string | null
}>()

const emit = defineEmits<{
  select: [id: string | null]
  hover: [edge: GraphEdge | null]
}>()

const container = ref<HTMLDivElement | null>(null)
let cy: Core | null = null

const PARTY_COLOURS = {
  INVESTOR: '#0284c7',
  SECURITY: '#7c3aed',
  BORROWER: '#ea580c',
  BANK: '#475569',
  ISSUER: '#64748b',
} as const

const style: StylesheetJson = [
  {
    selector: 'node',
    style: {
      'background-color': 'data(colour)',
      label: 'data(label)',
      'font-size': 10,
      color: '#334155',
      'text-valign': 'center',
      'text-halign': 'right',
      'text-margin-x': 6,
      width: 14,
      height: 14,
    },
  },
  // Labels sit on the side away from the money: Investors' on the left,
  // Borrowers' on the right, Securities' above.
  { selector: 'node[kind = "INVESTOR"]', style: { 'text-halign': 'left', 'text-margin-x': -6 } },
  {
    selector: 'node[kind = "SECURITY"]',
    style: { shape: 'round-rectangle', 'text-halign': 'center', 'text-valign': 'top', 'text-margin-y': -4, 'text-margin-x': 0 },
  },
  { selector: 'node[kind = "BORROWER"]', style: { shape: 'diamond', width: 18, height: 18 } },
  { selector: 'node[kind = "BANK"]', style: { shape: 'barrel', width: 22, height: 22, 'text-halign': 'left', 'text-margin-x': -6 } },
  {
    selector: 'edge',
    style: {
      width: 'data(width)',
      'line-color': 'data(colour)',
      'target-arrow-color': 'data(colour)',
      'target-arrow-shape': 'triangle',
      'arrow-scale': 0.6,
      'curve-style': 'bezier',
      opacity: 0.55,
    },
  },
  { selector: '.faded', style: { opacity: 0.06 } },
  { selector: 'edge.lit', style: { opacity: 0.9 } },
  { selector: 'node:selected', style: { 'border-width': 3, 'border-color': '#0f172a' } },
]

function width(edge: GraphEdge): number {
  if (props.largestMinorUnits <= 0n) return 1
  return 1 + 11 * Math.sqrt(Number(edge.amountMinorUnits) / Number(props.largestMinorUnits))
}

function nodeElement(node: GraphNode): ElementDefinition {
  return {
    group: 'nodes',
    data: { id: node.id, label: node.label, kind: node.kind, colour: PARTY_COLOURS[node.kind] },
    position: { ...(props.positions.get(node.id) ?? { x: 0, y: 0 }) },
  }
}

function sync() {
  if (!cy) return
  const graph = cy
  const wanted = new Set([...props.nodes.map((n) => n.id), ...props.edges.map((e) => e.id)])

  graph.batch(() => {
    graph.elements().forEach((element) => {
      if (!wanted.has(element.id())) element.remove()
    })
    for (const node of props.nodes) {
      const existing = graph.getElementById(node.id)
      if (existing.empty()) graph.add(nodeElement(node))
      else existing.position({ ...(props.positions.get(node.id) ?? existing.position()) })
    }
    for (const edge of props.edges) {
      const existing = graph.getElementById(edge.id)
      if (existing.empty()) {
        graph.add({
          group: 'edges',
          data: {
            id: edge.id,
            source: edge.source,
            target: edge.target,
            width: width(edge),
            colour: FLOW_COLOURS[flowOf(edge.kind)],
          },
        })
      } else {
        existing.data('width', width(edge))
      }
    }
  })
  highlight()
}

// Everything but the selected Party and the money in and out of it fades.
function highlight() {
  if (!cy) return
  cy.elements().removeClass('faded lit')
  if (!props.selected) return

  const focus = cy.getElementById(props.selected)
  if (focus.empty()) return
  const neighbourhood = focus.closedNeighborhood()
  cy.elements().not(neighbourhood).addClass('faded')
  neighbourhood.edges().addClass('lit')
}

onMounted(() => {
  cy = cytoscape({
    container: container.value,
    style,
    layout: { name: 'preset' },
    minZoom: 0.1,
    maxZoom: 3,
    wheelSensitivity: 0.3,
    boxSelectionEnabled: false,
    autoungrabify: true,
  })
  sync()
  cy.fit(undefined, 40)

  cy.on('tap', 'node', (event) => emit('select', event.target.id()))
  cy.on('tap', (event) => {
    if (event.target === cy) emit('select', null)
  })
  cy.on('mouseover', 'edge', (event) => {
    emit('hover', props.edges.find((e) => e.id === event.target.id()) ?? null)
  })
  cy.on('mouseout', 'edge', () => emit('hover', null))
})

onBeforeUnmount(() => {
  cy?.destroy()
  cy = null
})

watch(() => [props.nodes, props.edges, props.positions], sync)
watch(() => props.selected, highlight)

// Re-frames the whole graph, e.g. after the Securities are collapsed.
function fit() {
  cy?.fit(undefined, 40)
}

defineExpose({ fit })
</script>

<template>
  <div ref="container" class="h-full w-full" data-testid="money-flow-canvas" />
</template>
