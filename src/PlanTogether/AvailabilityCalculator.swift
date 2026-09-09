import Foundation

enum AvailabilityCalculator {
    static func commonSlots(
        members: [Member],
        syncedMemberIDs: Set<UUID>,
        busyIntervals: [BusyInterval],
        searchRange: DateInterval,
        duration: TimeInterval
    ) -> [AvailabilitySlot] {
        guard !members.isEmpty, duration > 0, searchRange.duration >= duration else { return [] }

        let memberIDs = Set(members.map(\.id))
        guard memberIDs.isSubset(of: syncedMemberIDs) else { return [] }
        let relevantBusy = busyIntervals.filter {
            memberIDs.contains($0.ownerID) && $0.start < searchRange.end && $0.end > searchRange.start
        }
        var boundaries = [searchRange.start, searchRange.end]
        for interval in relevantBusy {
            boundaries.append(max(interval.start, searchRange.start))
            boundaries.append(min(interval.end, searchRange.end))
        }
        boundaries = Array(Set(boundaries)).sorted()

        var freeSegments: [DateInterval] = []
        for index in 0..<(boundaries.count - 1) {
            let segment = DateInterval(start: boundaries[index], end: boundaries[index + 1])
            let busyOwners = Set(relevantBusy.filter {
                $0.start < segment.end && $0.end > segment.start
            }.map(\.ownerID))
            if busyOwners.isEmpty { freeSegments.append(segment) }
        }

        var merged: [DateInterval] = []
        for segment in freeSegments {
            if let last = merged.last, last.end == segment.start {
                merged[merged.count - 1] = DateInterval(start: last.start, end: segment.end)
            } else {
                merged.append(segment)
            }
        }

        return merged.flatMap { interval -> [AvailabilitySlot] in
            guard interval.duration >= duration else { return [] }
            var slots: [AvailabilitySlot] = []
            var cursor = interval.start
            while cursor.addingTimeInterval(duration) <= interval.end {
                slots.append(AvailabilitySlot(
                    start: cursor,
                    end: cursor.addingTimeInterval(duration),
                    availableMemberCount: members.count,
                    totalMemberCount: members.count
                ))
                cursor = cursor.addingTimeInterval(duration)
            }
            return slots
        }
    }
}
