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
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { GroupBadge } from '@/components/group-badge'
import { StatusBadge } from '@/components/status-badge'

import type { Model } from '../types'
import { ModelSquareStatus } from './model-square-status'

export function ModelConnections(props: { model: Model }) {
  const { t } = useTranslation()
  return (
    <div className='min-h-0 flex-1 space-y-6 overflow-auto p-4'>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Channel availability and group access are derived from enabled channels. Importing metadata does not create a callable channel.'
        )}
      </p>
      <ModelSquareStatus model={props.model} detail />
      <section className='space-y-3'>
        <h3 className='font-medium'>{t('Bound Channels')}</h3>
        {props.model.bound_channels?.length ? (
          <StaticDataTable
            data={props.model.bound_channels}
            columns={[
              {
                id: 'name',
                header: t('Channel'),
                cellClassName: 'align-top whitespace-normal',
                cell: (channel) => (
                  <div className='flex min-w-0 flex-col gap-1'>
                    <span className='break-words'>{channel.name}</span>
                    {channel.groups?.length ? (
                      <div className='flex min-w-0 flex-wrap gap-1.5'>
                        {channel.groups.map((group) => {
                          const name = group.trim()
                          return (
                            <GroupBadge
                              key={group}
                              group={group}
                              copyable={false}
                              className='h-auto max-w-full whitespace-normal break-words'
                            >
                              <span className='break-words whitespace-normal'>
                                {name === 'auto'
                                  ? t('Auto')
                                  : name || t('User Group')}
                              </span>
                            </GroupBadge>
                          )
                        })}
                      </div>
                    ) : null}
                  </div>
                ),
              },
              {
                id: 'type',
                header: t('Type'),
                cell: (channel) => channel.type,
              },
              {
                id: 'billing_source',
                header: t('Billing source'),
                cellClassName: 'align-top whitespace-normal',
                cell: (channel) => {
                  const isReturnedCost =
                    channel.billing_source === 'openrouter_cost'
                  let label = t('Unknown')
                  if (isReturnedCost) {
                    label = t('OpenRouter returned cost')
                  } else if (channel.billing_source === 'local') {
                    label = t('Local pricing')
                  }

                  return (
                    <StatusBadge
                      variant={isReturnedCost ? 'info' : 'neutral'}
                      copyable={false}
                      className='h-auto min-h-5 max-w-full whitespace-normal'
                    >
                      <span className='break-words whitespace-normal'>
                        {label}
                      </span>
                    </StatusBadge>
                  )
                },
              },
            ]}
          />
        ) : (
          <EmptyState title={t('No available channels')} />
        )}
        <p className='text-muted-foreground text-sm'>
          {t(
            'OpenRouter returned-cost billing is supported for text requests and uses the final upstream USD cost multiplied by the effective group multiplier.'
          )}
        </p>
      </section>
      <section className='space-y-3'>
        <h3 className='font-medium'>{t('Enable Groups')}</h3>
        <div className='flex flex-wrap gap-2'>
          {props.model.enable_groups?.map((group) => (
            <GroupBadge key={group} group={group} />
          ))}
          {!props.model.enable_groups?.length && (
            <span className='text-muted-foreground text-sm'>
              {t('No enabled groups')}
            </span>
          )}
        </div>
      </section>
      <section className='space-y-3'>
        <h3 className='font-medium'>{t('Supported endpoints')}</h3>
        <div className='flex flex-wrap gap-2'>
          {props.model.supported_endpoints?.map((endpoint) => (
            <StatusBadge key={endpoint} variant='neutral' label={endpoint} />
          ))}
          {!props.model.supported_endpoints?.length && (
            <span className='text-muted-foreground text-sm'>
              {t('No endpoints inferred from channels')}
            </span>
          )}
        </div>
      </section>
      {props.model.name_rule !== 0 && (
        <section className='space-y-3'>
          <h3 className='font-medium'>{t('Matched models')}</h3>
          <div className='flex flex-wrap gap-2'>
            {props.model.matched_models?.map((name) => (
              <StatusBadge key={name} variant='neutral' label={name} />
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
