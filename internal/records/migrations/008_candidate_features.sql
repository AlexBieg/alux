-- The raw parts of each candidate's score, comparable across drafts; null on rows logged before this step.
ALTER TABLE candidate_verdicts ADD COLUMN features TEXT CHECK (features IS NULL OR json_valid(features));
