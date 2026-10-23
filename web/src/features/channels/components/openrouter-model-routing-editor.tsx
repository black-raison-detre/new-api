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
import { useQuery } from '@tanstack/react-query'
import { useEffect, useId, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ComboboxInput } from '@/components/ui/combobox-input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { getServerErrorMessage } from '@/lib/server-error-message'

import {
  fetchOpenRouterEndpoints,
  type OpenRouterEndpointRequest,
} from '../api'
import {
  openRouterBaseModel,
  openRouterModelPolicySchema,
  type OpenRouterModelPolicy,
} from '../lib/openrouter-routing'

export type OpenRouterConnection = Omit<OpenRouterEndpointRequest, 'model'>

export function OpenRouterRoutingBadge(props: {
  policy?: OpenRouterModelPolicy
  upstream: string | null
}) {
  const { t } = useTranslation()
  let label = t('Standard')
  if (props.upstream === null) {
    label = t('Mapping cycle')
  } else if (
    !props.policy &&
    openRouterBaseModel(props.upstream) !== props.upstream
  ) {
    label = t('Existing mapping')
  } else if (props.policy?.mode === 'nitro') label = t('Nitro')
  else if (props.policy?.mode === 'floor') label = t('Floor')
  else if (props.policy?.mode === 'locked') {
    label = t('Locked: {{provider}}', {
      provider: props.policy.provider || t('Select provider'),
    })
  }
  return (
    <Badge
      variant='outline'
      className='max-w-full font-normal break-all whitespace-normal'
    >
      {label}
    </Badge>
  )
}

function ProviderEndpoint(props: {
  model: string
  connection: OpenRouterConnection
  value: string
  invalid: boolean
  onChange: (value: string) => void
}) {
  const { t } = useTranslation()
  const id = useId()
  // Keep credentials out of query keys and discard results after this editor closes.
  const [request, setRequest] = useState({
    connection: props.connection,
    revision: 0,
  })
  if (request.connection !== props.connection) {
    setRequest({ connection: props.connection, revision: request.revision + 1 })
  }
  const query = useQuery({
    queryKey: ['openrouter-endpoints', id, props.model, request.revision],
    queryFn: ({ signal }) =>
      fetchOpenRouterEndpoints(
        { ...props.connection, model: props.model },
        signal
      ),
    retry: false,
    gcTime: 0,
    meta: { errorToast: false },
  })
  const options = (query.data?.endpoints ?? []).map((endpoint) => ({
    value: endpoint.id,
    label: endpoint.name || endpoint.id,
    description: `${endpoint.provider_name} · ${endpoint.id}`,
  }))
  const unavailable = Boolean(
    props.value &&
    query.isSuccess &&
    !options.some((option) => option.value === props.value)
  )
  return (
    <div className='space-y-2'>
      <Label htmlFor={id}>{t('Provider endpoint')}</Label>
      <ComboboxInput
        id={id}
        options={options}
        value={props.value}
        onValueChange={props.onChange}
        allowCustomValue
        placeholder={t('Search or enter an exact endpoint ID')}
        emptyText={t('No matching provider endpoints')}
        aria-invalid={props.invalid}
      />
      <p className='text-muted-foreground text-xs'>
        {t(
          'This model uses only the selected endpoint. Other providers are not used as fallback.'
        )}
      </p>
      {query.isPending && (
        <LoadingState inline message={t('Loading provider endpoints...')} />
      )}
      {query.isError && (
        <ErrorState
          className='min-h-0 p-3'
          title={t('Failed to load provider endpoints')}
          description={getServerErrorMessage(
            query.error,
            t('You can enter an exact endpoint ID manually.')
          )}
          onRetry={() => {
            void query.refetch()
          }}
        />
      )}
      {query.isSuccess && options.length === 0 && (
        <EmptyState
          className='min-h-0 p-3'
          title={t('No provider endpoints returned for this model')}
          description={t('You can enter an exact endpoint ID manually.')}
        />
      )}
      {unavailable && (
        <Alert>
          <AlertDescription>
            {t(
              'The configured endpoint is not in the current list. Its exact ID is preserved.'
            )}
          </AlertDescription>
        </Alert>
      )}
      {query.data?.stale && (
        <Alert>
          <AlertDescription>
            {t(
              'Provider information may be out of date. Refresh before choosing a new endpoint.'
            )}
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={() => {
                void query.refetch()
              }}
            >
              {t('Retry')}
            </Button>
          </AlertDescription>
        </Alert>
      )}
    </div>
  )
}

export function OpenRouterModelRoutingEditor(props: {
  model: string
  upstream: string
  policy?: OpenRouterModelPolicy
  connection: OpenRouterConnection
  disabled?: boolean
  onChange: (policy: OpenRouterModelPolicy) => void
  onAddAlias: () => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const id = useId()
  const title = useRef<HTMLHeadingElement>(null)
  useEffect(() => {
    title.current?.focus()
  }, [props.model])
  const legacy =
    !props.policy && openRouterBaseModel(props.upstream) !== props.upstream
  const mode = legacy ? 'legacy' : (props.policy?.mode ?? 'standard')
  const policyError = props.policy
    ? openRouterModelPolicySchema.safeParse(props.policy)
    : null
  const items = [
    { value: 'standard', label: t('Standard') },
    { value: 'nitro', label: t('Nitro') },
    { value: 'floor', label: t('Floor') },
    { value: 'locked', label: t('Locked provider') },
  ]
  if (legacy) items.unshift({ value: 'legacy', label: t('Existing mapping') })
  return (
    <section
      className='bg-muted/30 space-y-4 rounded-lg border p-3 sm:p-4'
      aria-labelledby={id}
    >
      <div className='flex items-start justify-between gap-3'>
        <div className='min-w-0 space-y-1'>
          <h3
            ref={title}
            tabIndex={-1}
            id={id}
            className='focus-visible:ring-ring text-sm font-medium break-all outline-none focus-visible:ring-2'
          >
            {t('Routing for {{model}}', { model: props.model })}
          </h3>
          <p className='text-muted-foreground text-xs break-all'>
            {t('Upstream model')}: {props.upstream}
          </p>
        </div>
        <Button type='button' variant='ghost' size='sm' onClick={props.onClose}>
          {t('Close')}
        </Button>
      </div>
      <fieldset
        disabled={props.disabled}
        className='min-w-0 space-y-4 disabled:opacity-60'
      >
        <div className='space-y-2'>
          <Label htmlFor={`${id}-mode`}>{t('OpenRouter routing')}</Label>
          <Select
            items={items}
            value={mode}
            disabled={props.disabled}
            onValueChange={(value) => {
              if (
                value === 'standard' ||
                value === 'nitro' ||
                value === 'floor' ||
                value === 'locked'
              ) {
                props.onChange(
                  value === 'locked'
                    ? { mode: value, provider: props.policy?.provider ?? '' }
                    : { mode: value }
                )
              }
            }}
          >
            <SelectTrigger id={`${id}-mode`} className='w-full'>
              <SelectValue />
            </SelectTrigger>
            <SelectContent alignItemWithTrigger={false}>
              {items.map((item) => (
                <SelectItem
                  key={item.value}
                  value={item.value}
                  disabled={item.value === 'legacy'}
                >
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {mode === 'standard' && (
            <p className='text-muted-foreground text-xs'>
              {t('Use OpenRouter default routing for this model.')}
            </p>
          )}
          {mode === 'nitro' && (
            <p className='text-muted-foreground text-xs'>
              {t('Nitro includes priority endpoints and prefers throughput.')}
            </p>
          )}
          {mode === 'floor' && (
            <p className='text-muted-foreground text-xs'>
              {t('Floor includes flex endpoints and prefers price.')}
            </p>
          )}
          {legacy && (
            <p className='text-muted-foreground text-xs'>
              {t(
                'The existing routing suffix is preserved until you choose a routing mode.'
              )}
            </p>
          )}
        </div>
        {mode === 'locked' && !props.disabled && (
          <ProviderEndpoint
            model={openRouterBaseModel(props.upstream)}
            connection={props.connection}
            value={props.policy?.provider ?? ''}
            invalid={Boolean(policyError && !policyError.success)}
            onChange={(provider) =>
              props.onChange({ mode: 'locked', provider: provider.trim() })
            }
          />
        )}
        {mode === 'locked' && props.disabled && (
          <p className='text-sm break-all'>
            {t('Provider endpoint')}: {props.policy?.provider}
          </p>
        )}
        {policyError && !policyError.success && (
          <p role='alert' className='text-destructive text-sm'>
            {t(policyError.error.issues[0].message)}
          </p>
        )}
        <Button
          type='button'
          variant='outline'
          size='sm'
          disabled={props.disabled}
          onClick={props.onAddAlias}
        >
          {t('Add another name for this model')}
        </Button>
      </fieldset>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Conversation routing stays with a provider when possible to help cache reuse. Provider availability and cache hits are not guaranteed.'
        )}
      </p>
    </section>
  )
}
