/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { z } from 'zod'

const providerEndpointPattern =
  /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}(?:\/[A-Za-z0-9][A-Za-z0-9._-]{0,127}){0,3}$/

export const openRouterModelPolicySchema = z
  .object({
    mode: z.enum(['standard', 'nitro', 'floor', 'locked']),
    provider: z.string().trim().optional(),
  })
  .superRefine((policy, context) => {
    if (policy.mode === 'locked' && !policy.provider?.trim()) {
      context.addIssue({
        code: 'custom',
        path: ['provider'],
        message: 'Select a provider endpoint for locked routing.',
      })
    }
    if (
      policy.provider &&
      (policy.provider.length > 256 ||
        !providerEndpointPattern.test(policy.provider))
    ) {
      context.addIssue({
        code: 'custom',
        path: ['provider'],
        message: 'Enter a valid provider endpoint ID (up to 256 characters).',
      })
    }
    if (policy.mode !== 'locked' && policy.provider) {
      context.addIssue({
        code: 'custom',
        path: ['provider'],
        message: 'Provider endpoints are only allowed with locked routing.',
      })
    }
  })

export const openRouterModelRoutingSchema = z.record(
  z.string(),
  openRouterModelPolicySchema
)
export type OpenRouterModelPolicy = z.infer<typeof openRouterModelPolicySchema>
export type OpenRouterModelRouting = z.infer<
  typeof openRouterModelRoutingSchema
>

/** Remove only routing variants; other catalog variants retain their meaning. */
export function openRouterBaseModel(model: string): string {
  return model
    .split(':')
    .filter(
      (segment, index) =>
        index === 0 || !['nitro', 'floor'].includes(segment.toLowerCase())
    )
    .join(':')
}

/** Follow explicit aliases like the relay, accepting terminal self-mappings. */
export function resolveOpenRouterModel(
  model: string,
  mapping: Readonly<Record<string, string>>
): string | null {
  const visited = new Set([model])
  let current = model
  while (Object.hasOwn(mapping, current)) {
    const next = mapping[current]
    if (!next || next === current) return current
    if (visited.has(next)) return null
    visited.add(next)
    current = next
  }
  return current
}
