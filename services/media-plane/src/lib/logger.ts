import winston from 'winston';
import { config } from '../config/index.js';

const format = winston.format.combine(
  winston.format.timestamp(),
  winston.format.errors({ stack: true }),
  config.environment === 'production'
    ? winston.format.json()
    : winston.format.combine(
        winston.format.colorize(),
        winston.format.printf(({ level, message, timestamp, ...meta }) => {
          const metaStr = Object.keys(meta).length ? JSON.stringify(meta, null, 2) : '';
          return `${timestamp} [${level}]: ${message} ${metaStr}`;
        })
      )
);

export const logger = winston.createLogger({
  level: process.env.LOG_LEVEL || (config.environment === 'production' ? 'info' : 'debug'),
  format,
  transports: [
    new winston.transports.Console(),
  ],
});
