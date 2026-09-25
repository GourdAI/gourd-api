import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { create, normalizeGroupIds, update } from '../keys'

const { post, put } = vi.hoisted(() => ({ post: vi.fn(), put: vi.fn() }))
vi.mock('../client', () => ({ apiClient: { post, put } }))

/**
 * Frozen backend contract for multi-group API keys:
 * - `group_ids` always wins over `group_id` when both are present.
 * - a single `group_id` is equivalent to a one-element `group_ids`.
 * - `group_ids: []` unbinds every group.
 */
describe('API key multi-group contract', () => {
  beforeEach(() => {
    post.mockReset()
    put.mockReset()
    post.mockResolvedValue({ data: { id: 1 } })
    put.mockResolvedValue({ data: { id: 1 } })
  })

  describe('normalizeGroupIds', () => {
    it('accepts an array, a single id and empty input', () => {
      expect(normalizeGroupIds([3, 1, 2])).toEqual([3, 1, 2])
      expect(normalizeGroupIds(7)).toEqual([7])
      expect(normalizeGroupIds(null)).toEqual([])
      expect(normalizeGroupIds(undefined)).toEqual([])
      expect(normalizeGroupIds([])).toEqual([])
    })

    it('preserves order while dropping duplicates and invalid ids', () => {
      expect(normalizeGroupIds([5, 5, 0, -1, 9])).toEqual([5, 9])
    })
  })

  describe('create', () => {
    it('sends group_ids and mirrors the primary group into group_id', async () => {
      await create('Multi', [5, 6])

      expect(post).toHaveBeenCalledWith('/keys', expect.objectContaining({
        name: 'Multi',
        group_ids: [5, 6],
        group_id: 5
      }))
    })

    it('still accepts the legacy single group id', async () => {
      await create('Legacy', 4)

      expect(post).toHaveBeenCalledWith('/keys', expect.objectContaining({
        name: 'Legacy',
        group_ids: [4],
        group_id: 4
      }))
    })

    it('omits both group fields when no group is given', async () => {
      await create('Ungrouped', null)

      const payload = post.mock.calls[0][1]
      expect(payload).not.toHaveProperty('group_ids')
      expect(payload).not.toHaveProperty('group_id')
    })

    it('omits both group fields for a selection of only invalid ids', async () => {
      await create('Ungrouped', [0, -3])

      const payload = post.mock.calls[0][1]
      expect(payload).not.toHaveProperty('group_ids')
      expect(payload).not.toHaveProperty('group_id')
    })
  })

  describe('update', () => {
    it('forwards a full group_ids replacement untouched', async () => {
      await update(9, { group_ids: [2, 1] })

      expect(put).toHaveBeenCalledWith('/keys/9', { group_ids: [2, 1] })
    })

    it('supports unbinding every group with an empty array', async () => {
      await update(9, { group_ids: [] })

      expect(put).toHaveBeenCalledWith('/keys/9', { group_ids: [] })
    })

    it('keeps the legacy single group_id payload intact', async () => {
      await update(9, { group_id: 3 })

      expect(put).toHaveBeenCalledWith('/keys/9', { group_id: 3 })
    })
  })
})
