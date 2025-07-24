// MongoDB initialization script
// This script runs when the MongoDB container starts for the first time

// Switch to the game database
db = db.getSiblingDB('game');

// Create a simple collection to ensure the database exists
db.createCollection('init');

// You can add more initialization logic here if needed
print('MongoDB initialization completed successfully');