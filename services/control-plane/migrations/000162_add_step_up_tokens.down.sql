-- Reverses 000162. Every row is a token at most 60 seconds from expiry, so
-- dropping them costs at most one re-prompt per user who held one; nothing
-- durable is lost. DROP TABLE drops the table's indexes with it.
DROP TABLE IF EXISTS public.step_up_tokens;
